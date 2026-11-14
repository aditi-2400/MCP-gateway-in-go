package metrics

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit"
)

func newTestMetrics() *Metrics {
	tools := map[string]bool{"fs__read": true, "email__send": true}
	agents := map[string]bool{"ci-bot": true, "analyst": true}
	return New(func(t string) bool { return tools[t] }, func(a string) bool { return agents[a] })
}

func entry(agent, tool string, d audit.Decision, s audit.Status, latency time.Duration) audit.Entry {
	return audit.Entry{Agent: agent, Tool: tool, Decision: d, Status: s, Latency: latency}
}

func TestCallDoneCounts(t *testing.T) {
	m := newTestMetrics()
	m.CallDone(entry("ci-bot", "fs__read", audit.Allowed, audit.StatusOK, 3*time.Millisecond), 4*time.Millisecond)
	m.CallDone(entry("ci-bot", "fs__read", audit.Allowed, audit.StatusOK, 3*time.Millisecond), 4*time.Millisecond)
	m.CallDone(entry("ci-bot", "fs__read", audit.Denied, audit.StatusToolError, 0), time.Millisecond)
	m.CallDone(entry("analyst", "email__send", audit.RateLimited, audit.StatusRPCError, 0), time.Millisecond)
	m.CallDone(entry("analyst", "fs__delete", audit.Denied, audit.StatusRPCError, 0), time.Millisecond)

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"allowed fs__read", testutil.ToFloat64(m.calls.WithLabelValues("ci-bot", "fs__read", "allowed", "ok")), 2},
		{"denied by arguments", testutil.ToFloat64(m.denials.WithLabelValues("ci-bot", "fs__read", "arguments")), 1},
		{"rate limited", testutil.ToFloat64(m.denials.WithLabelValues("analyst", "email__send", "rate_limited")), 1},
		{"unknown tool is other", testutil.ToFloat64(m.denials.WithLabelValues("analyst", "other", "tool")), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// Only the two forwarded calls have an upstream latency.
	if n := testutil.CollectAndCount(m.upstream); n != 1 {
		t.Errorf("upstream latency series = %d, want 1 (fs__read only)", n)
	}
	// Allowed calls are not denials.
	if n := testutil.CollectAndCount(m.denials); n != 3 {
		t.Errorf("denial series = %d, want 3", n)
	}
}

// Made-up tool and agent names must not create new series.
func TestLabelsStayBounded(t *testing.T) {
	m := newTestMetrics()
	for i := range 1000 {
		m.CallDone(entry(fmt.Sprintf("agent%d", i), fmt.Sprintf("fs__made_up_%d", i), audit.Denied, audit.StatusRPCError, 0), time.Millisecond)
	}
	if n := testutil.CollectAndCount(m.calls); n != 1 {
		t.Fatalf("%d call series after 1000 made-up names, want 1", n)
	}
	if got := testutil.ToFloat64(m.calls.WithLabelValues("none", "other", "denied", "rpc_error")); got != 1000 {
		t.Errorf("none/other count = %v, want 1000", got)
	}
}

func TestDenialReason(t *testing.T) {
	tests := []struct {
		d    audit.Decision
		s    audit.Status
		want string
	}{
		{audit.Allowed, audit.StatusOK, ""},
		{audit.Allowed, audit.StatusToolError, ""}, // the tool failed; the gateway didn't refuse
		{audit.Denied, audit.StatusToolError, "arguments"},
		{audit.Denied, audit.StatusRPCError, "tool"},
		{audit.RateLimited, audit.StatusRPCError, "rate_limited"},
	}
	for _, tt := range tests {
		if got := denialReason(audit.Entry{Decision: tt.d, Status: tt.s}); got != tt.want {
			t.Errorf("denialReason(%s, %s) = %q, want %q", tt.d, tt.s, got, tt.want)
		}
	}
}

func TestHandlerServesEverything(t *testing.T) {
	m := newTestMetrics()
	used := 3
	m.WatchAuditQueue(func() int { return used }, func() int { return 1024 })
	m.CallDone(entry("ci-bot", "fs__read", audit.Allowed, audit.StatusOK, 2*time.Millisecond), 3*time.Millisecond)
	m.CallRefused()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	for _, want := range []string{
		`mcpgw_tool_calls_total{agent="ci-bot",decision="allowed",status="ok",tool="fs__read"} 1`,
		`mcpgw_tool_call_duration_seconds_bucket{decision="allowed",le="0.005"} 1`,
		`mcpgw_upstream_latency_seconds_count{tool="fs__read"} 1`,
		`mcpgw_audit_refused_total 1`,
		`mcpgw_audit_queue_slots_used 3`,
		`mcpgw_audit_queue_slots 1024`,
		`go_goroutines `,
		`process_`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output has no %q", want)
		}
	}
}
