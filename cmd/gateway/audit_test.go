package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit" // also registers the sqlite driver
)

const auditPolicy = `
roles:
  r:
    allow:
      - tool: alpha__add
      - tool: alpha__fail
      - tool: alpha__echo
        args:
          text: { max_length: 40 }
agents:
  tester: [r]
limits:
  default: { rate: 1000/second, burst: 1000 }
  tools:
    - { tool: alpha__add, rate: 1/hour, burst: 1 }
`

// auditRows reads every row as "tool|decision|status|error_code|arguments".
func auditRows(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT agent, tool, decision, status, error_code, arguments, request_id, time, latency_us FROM audit_log ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var agent, tool, decision, status, args, id, tm string
		var code sql.NullInt64
		var latency int64
		if err := rows.Scan(&agent, &tool, &decision, &status, &code, &args, &id, &tm, &latency); err != nil {
			t.Fatal(err)
		}
		if agent != "tester" || id == "" || len(tm) != len(audit.TimeLayout) {
			t.Errorf("row for %s: agent %q, id %q, time %q", tool, agent, id, tm)
		}
		// Calls that reached the upstream took some time there; blocked ones none.
		reached := decision == "allowed" && tool != "alpha__count"
		if reached != (latency > 0) {
			t.Errorf("row for %s (%s): latency %dus, want it above zero only for calls that reached the upstream", tool, decision, latency)
		}
		codeText := ""
		if code.Valid {
			codeText = strconv.FormatInt(code.Int64, 10)
		}
		out = append(out, strings.Join([]string{tool, decision, status, codeText, args}, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunAuditsEveryToolCall(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, auditPolicy)
	g := startGateway(t, context.Background(), "-config", cfg)

	secret := "sk-" + strings.Repeat("q7", 12) // built at runtime, see the audit tests
	calls := []struct{ tool, args string }{
		{"alpha__add", `{"a":1,"b":2}`},                               // allowed, ok
		{"alpha__add", `{"a":1,"b":2}`},                               // rate limited
		{"alpha__echo", `{"text":"key ` + secret + `"}`},              // allowed, secret redacted
		{"alpha__echo", `{"text":"` + strings.Repeat("x", 41) + `"}`}, // arguments denied
		{"alpha__count", `{"n":1}`},                                   // tool denied
		{"alpha__fail", `{}`},                                         // allowed, tool error
	}
	for i, c := range calls {
		g.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i+1) + `,"method":"tools/call","params":` + callParams(c.tool, c.args) + `}`)
		g.next()
	}
	// tools/list is not audited.
	g.send(`{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\n%s", err, g.stderr)
	}

	wantAuditClosed(t, cfg)
	got := auditRows(t, auditDBFor(cfg))
	want := []string{
		`alpha__add|allowed|ok||{"a":1,"b":2}`,
		`alpha__add|rate_limited|rpc_error|-31029|{"a":1,"b":2}`,
		`alpha__echo|allowed|ok||{"text":"key [REDACTED]"}`,
		`alpha__echo|denied|tool_error||{"text":"` + strings.Repeat("x", 41) + `"}`,
		`alpha__count|denied|rpc_error|-32602|{"n":1}`,
		`alpha__fail|allowed|tool_error||{}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit rows:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(got, ""), secret) {
		t.Error("audit log contains the secret")
	}
}

func TestRunHTTPAuditsCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, auditPolicy)
	g := startGateway(t, ctx, "-config", cfg, "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	for i := 1; i <= 2; i++ {
		postMCP(t, url, "tools/call", "alpha__add", `{"jsonrpc":"2.0","id":`+strconv.Itoa(i)+`,"method":"tools/call","params":`+callParams("alpha__add", `{"a":1,"b":2}`)+`}`)
	}
	cancel()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	wantAuditClosed(t, cfg)
	got := auditRows(t, auditDBFor(cfg))
	want := []string{
		`alpha__add|allowed|ok||{"a":1,"b":2}`,
		`alpha__add|rate_limited|rpc_error|-31029|{"a":1,"b":2}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit rows:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRunRefusesBadAuditPathBeforeStartingAnything(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	bad := filepath.Join(t.TempDir(), "no-such-dir", "audit.db")
	stderr := &syncBuffer{}
	err := run(context.Background(), withTestFiles([]string{"-config", cfg, "-audit-db", bad}), strings.NewReader(""), io.Discard, stderr, testEnv)
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("run() error = %v, want one naming %s", err, bad)
	}
	if strings.Contains(stderr.String(), "upstream started") {
		t.Error("an upstream started before the audit log was opened")
	}
}

func TestRunKeepsAuditAcrossRestarts(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, auditPolicy)
	for run := range 2 {
		g := startGateway(t, context.Background(), "-config", cfg)
		g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + callParams("alpha__fail", `{"run":`+fmt.Sprint(run)+`}`) + `}`)
		g.next()
		g.in.Close()
		if err := g.wait(); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if got := auditRows(t, auditDBFor(cfg)); len(got) != 2 {
		t.Errorf("rows after two runs = %v, want 2", got)
	}
	if _, err := os.Stat(auditDBFor(cfg)); err != nil {
		t.Fatal(err)
	}
}

// wantAuditClosed checks the gateway closed the audit log on the way out.
// SQLite removes the -wal file when the last connection closes cleanly, so
// it is still there if shutdown skipped the flush and close.
func wantAuditClosed(t *testing.T, cfg string) {
	t.Helper()
	if _, err := os.Stat(auditDBFor(cfg) + "-wal"); err == nil {
		t.Error("audit.db-wal still exists: the audit log was not closed at shutdown")
	}
}
