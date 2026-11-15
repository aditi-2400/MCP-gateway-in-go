package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/ratelimit"
)

// burstLimitPolicy lets tester call alpha__add 5 times before the limit
// starts; refilling one token an hour means none come back during the test.
// alpha__count is not allowed at all.
const burstLimitPolicy = `
roles:
  r:
    allow:
      - tool: alpha__add
agents:
  tester: [r]
limits:
  default: { rate: 1000/second, burst: 1000 }
  tools:
    - { tool: alpha__add, rate: 1/hour, burst: 5 }
`

const (
	burstAdds   = 20
	burstLimit  = 5
	burstDenied = 5
)

// A burst sent all at once is allowed up to the limit, and the replies, the
// audit log, and the metrics agree on which calls were allowed, denied, and
// rate limited.
func TestRunBurstShowsInRepliesAuditAndMetrics(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, burstLimitPolicy)
	g := startGateway(t, context.Background(), "-config", cfg, "-metrics", "127.0.0.1:0")
	base := waitForMetrics(t, g.stderr)

	// Listing tools first lets the metrics use real tool names as labels.
	g.send(`{"jsonrpc":"2.0","id":0,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()

	// Every line is written before any reply is read, so the gateway handles
	// the calls at the same time. Each add carries its id in "a", so its
	// audit row can be matched to its reply.
	var lines []string
	for id := 1; id <= burstAdds; id++ {
		lines = append(lines, `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"method":"tools/call","params":`+callParams("alpha__add", fmt.Sprintf(`{"a":%d,"b":0}`, id))+`}`)
	}
	for id := 101; id < 101+burstDenied; id++ {
		lines = append(lines, `{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+`,"method":"tools/call","params":`+callParams("alpha__count", `{"n":1}`)+`}`)
	}
	// Written from another goroutine: the reply channel is smaller than the
	// burst, so writing everything before reading could block.
	writeErr := make(chan error, 1)
	go func() {
		_, err := io.WriteString(g.in, strings.Join(lines, "\n")+"\n")
		writeErr <- err
	}()

	// What each reply says happened, by request id.
	replied := map[int]string{}
	for range lines {
		resp := g.next()
		id, err := strconv.Atoi(resp.ID.String())
		if err != nil {
			t.Fatalf("reply with id %s: %v", resp.ID.String(), err)
		}
		switch {
		case resp.Error == nil:
			replied[id] = "allowed"
		case resp.Error.Code == ratelimit.CodeRateLimited:
			replied[id] = "rate_limited"
		case id > burstAdds:
			replied[id] = "denied"
		default:
			t.Fatalf("call %d: unexpected error %+v", id, resp.Error)
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("client write: %v", err)
	}

	counts := map[string]int{}
	for id, got := range replied {
		counts[got]++
		if id > burstAdds && got != "denied" {
			t.Errorf("alpha__count call %d was %s, want denied", id, got)
		}
	}
	want := map[string]int{"allowed": burstLimit, "rate_limited": burstAdds - burstLimit, "denied": burstDenied}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("replies = %v, want %v", counts, want)
	}

	// Metrics are updated before each reply is written, so they are complete.
	status, text := scrape(t, http.MethodGet, base+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", status)
	}
	for _, line := range []string{
		fmt.Sprintf(`mcpgw_tool_calls_total{agent="tester",decision="allowed",status="ok",tool="alpha__add"} %d`, burstLimit),
		fmt.Sprintf(`mcpgw_tool_calls_total{agent="tester",decision="rate_limited",status="rpc_error",tool="alpha__add"} %d`, burstAdds-burstLimit),
		fmt.Sprintf(`mcpgw_tool_calls_total{agent="tester",decision="denied",status="rpc_error",tool="alpha__count"} %d`, burstDenied),
		fmt.Sprintf(`mcpgw_tool_call_denials_total{agent="tester",reason="rate_limited",tool="alpha__add"} %d`, burstAdds-burstLimit),
		fmt.Sprintf(`mcpgw_tool_call_denials_total{agent="tester",reason="tool",tool="alpha__count"} %d`, burstDenied),
		fmt.Sprintf(`mcpgw_upstream_latency_seconds_count{tool="alpha__add"} %d`, burstLimit),
	} {
		if !strings.Contains(text, line) {
			t.Errorf("metrics have no %q", line)
		}
	}

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\n%s", err, g.stderr)
	}

	// The audit log has one row per call, each with the decision its reply showed.
	wantAuditClosed(t, cfg)
	rows := auditRows(t, auditDBFor(cfg))
	if len(rows) != len(lines) {
		t.Fatalf("audit has %d rows, want %d:\n%s", len(rows), len(lines), strings.Join(rows, "\n"))
	}
	addRows := 0
	for _, row := range rows {
		fields := strings.Split(row, "|") // tool|decision|status|error_code|arguments
		tool, decision := fields[0], fields[1]
		if tool == "alpha__count" {
			if decision != "denied" {
				t.Errorf("audit row %q, want denied", row)
			}
			continue
		}
		addRows++
		var id int
		if _, err := fmt.Sscanf(fields[4], `{"a":%d,"b":0}`, &id); err != nil {
			t.Fatalf("audit row %q: arguments: %v", row, err)
		}
		if decision != replied[id] {
			t.Errorf("call %d: audit says %s, reply said %s", id, decision, replied[id])
		}
	}
	if addRows != burstAdds {
		t.Errorf("audit has %d alpha__add rows, want %d", addRows, burstAdds)
	}
}
