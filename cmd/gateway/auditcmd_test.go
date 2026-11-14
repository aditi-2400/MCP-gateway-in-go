package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit"
)

// seedAuditDB writes a few entries straight through the store.
func seedAuditDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	store, err := audit.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	entries := []audit.Entry{
		{Time: now.Add(-2 * time.Hour), RequestID: "old", Agent: "ci-bot", Tool: "fs__read_file", Arguments: json.RawMessage(`{"path":"/tmp/a"}`), Decision: audit.Allowed, Reason: "ok", Rule: "r: fs__read_file", Status: audit.StatusOK, Latency: 1500 * time.Microsecond},
		{Time: now.Add(-time.Minute), RequestID: "limited", Agent: "analyst", Tool: "db__query", Arguments: json.RawMessage(`{"sql":"select 1"}`), Decision: audit.RateLimited, Reason: "rate limit 5/minute, burst 2; retry in 12s", Rule: "db__query", Status: audit.StatusRPCError, ErrorCode: -31029},
		{Time: now, RequestID: "evil", Agent: "ci-bot", Tool: "fs__x\x1b[31mred\ttab", Arguments: json.RawMessage(`{"text":"` + strings.Repeat("y", 200) + `"}`), Decision: audit.Denied, Reason: "no role\x1b[2J", Status: audit.StatusRPCError, ErrorCode: -32602},
	}
	if err := store.Write(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	return path
}

func runAuditCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), append([]string{"audit"}, args...), strings.NewReader(""), &stdout, &stderr, testEnv)
	return stdout.String(), stderr.String(), err
}

func TestAuditCommandTable(t *testing.T) {
	path := seedAuditDB(t)
	out, _, err := runAuditCmd(t, "-db", path)
	if err != nil {
		t.Fatalf("audit error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "TIME") {
		t.Fatalf("output:\n%s\nwant a header and 3 rows", out)
	}
	for i, want := range []string{"fs__read_file", "db__query", "fs__x"} {
		if !strings.Contains(lines[i+1], want) {
			t.Errorf("row %d = %q, want %s (oldest first)", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(lines[2], "rpc_error -31029") || !strings.Contains(lines[1], "1.5ms") {
		t.Errorf("status or latency missing:\n%s", out)
	}
	// Agent-supplied text is escaped, so it can't drive the terminal.
	if strings.ContainsAny(out, "\x1b\t") {
		t.Errorf("output contains raw escape or tab characters:\n%q", out)
	}
	if !strings.Contains(lines[3], `fs__x\u001b[31mred\u0009tab`) || !strings.Contains(lines[3], `no role\u001b[2J`) {
		t.Errorf("row 3 = %q, want the control characters escaped", lines[3])
	}
	if !strings.Contains(lines[3], "…") || strings.Contains(lines[3], strings.Repeat("y", 100)) {
		t.Errorf("row 3 = %q, want long arguments cut short", lines[3])
	}
}

func TestAuditCommandFilters(t *testing.T) {
	path := seedAuditDB(t)
	tests := []struct {
		args []string
		want []string // request IDs, via -json
	}{
		{args: []string{"-agent", "ci-bot"}, want: []string{"old", "evil"}},
		{args: []string{"-tool", "db__*"}, want: []string{"limited"}},
		{args: []string{"-decision", "rate_limited"}, want: []string{"limited"}},
		{args: []string{"-since", "1h"}, want: []string{"limited", "evil"}},
		{args: []string{"-limit", "1"}, want: []string{"evil"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			out, _, err := runAuditCmd(t, append([]string{"-db", path, "-json"}, tt.args...)...)
			if err != nil {
				t.Fatalf("audit error = %v", err)
			}
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				var r jsonRecord
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatalf("line %q: %v", line, err)
				}
				got = append(got, r.RequestID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("rows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuditCommandJSON(t *testing.T) {
	path := seedAuditDB(t)
	out, _, err := runAuditCmd(t, "-db", path, "-json", "-decision", "rate_limited")
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if r["error_code"] != float64(-31029) || r["rule"] != "db__query" || r["arguments"].(map[string]any)["sql"] != "select 1" {
		t.Errorf("record = %v", r)
	}

	out, _, _ = runAuditCmd(t, "-db", path, "-json", "-agent", "ci-bot", "-tool", "fs__read_file")
	if strings.Contains(out, "error_code") {
		t.Errorf("ok row has an error_code: %s", out)
	}
}

func TestAuditCommandErrors(t *testing.T) {
	path := seedAuditDB(t)
	missing := filepath.Join(t.TempDir(), "missing.db")
	tests := []struct {
		name  string
		args  []string
		usage bool   // exit code 2
		want  string // in the error
	}{
		{name: "no db", args: nil, usage: true},
		{name: "zero limit", args: []string{"-db", path, "-limit", "0"}, usage: true},
		{name: "bad since", args: []string{"-db", path, "-since", "yesterday"}, usage: true},
		{name: "negative since", args: []string{"-db", path, "-since", "-1h"}, usage: true},
		{name: "extra args", args: []string{"-db", path, "more"}, usage: true},
		{name: "bad decision", args: []string{"-db", path, "-decision", "maybe"}, want: "decision"},
		{name: "partial wildcard", args: []string{"-db", path, "-tool", "fs__read_*"}, want: "upstream__*"},
		{name: "missing file", args: []string{"-db", missing}, want: "missing.db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := runAuditCmd(t, tt.args...)
			if err == nil {
				t.Fatal("error = nil, want one")
			}
			if errors.Is(err, errUsage) != tt.usage {
				t.Errorf("error = %v, usage = %v, want usage %v", err, errors.Is(err, errUsage), tt.usage)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"1h", now.Add(-time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"2026-11-14T10:00:00Z", time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)},
		{"2026-11-14T10:00:00+05:30", time.Date(2026, 11, 14, 4, 30, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		got, err := parseSince(tt.in, now)
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
}

// The command reads while a gateway is still running and writing.
func TestAuditCommandReadsALiveLog(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	g := startGateway(t, context.Background(), "-config", cfg)
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + callParams("alpha__add", `{"a":1,"b":2}`) + `}`)
	g.next()

	deadline := time.Now().Add(testTimeout)
	for {
		out, _, err := runAuditCmd(t, "-db", auditDBFor(cfg))
		if err == nil && strings.Contains(out, "alpha__add") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no row while the gateway runs: err %v, output:\n%s", err, out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The gateway keeps writing after the read.
	g.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` + callParams("alpha__add", `{"a":2,"b":2}`) + `}`)
	if resp := g.next(); resp.Error != nil {
		t.Fatalf("call after the read: %v", resp.Error)
	}
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	out, _, err := runAuditCmd(t, "-db", auditDBFor(cfg), "-json")
	if err != nil || strings.Count(out, "\n") != 2 {
		t.Errorf("after shutdown: %v, output:\n%s; want 2 rows", err, out)
	}
}

func TestPrintableAndShorten(t *testing.T) {
	if got := printable("a\x00b\x7fc\u0085d"); got != `a\u0000b\u007fc\u0085d` {
		t.Errorf("printable = %q", got)
	}
	if got := printable("héllo 日本"); got != "héllo 日本" {
		t.Errorf("printable changed plain text: %q", got)
	}
	if got := shorten("日本語テキスト", 4); got != "日本語…" {
		t.Errorf("shorten = %q, want it cut by characters", got)
	}
	if got := shorten("short", 10); got != "short" {
		t.Errorf("shorten = %q", got)
	}
}
