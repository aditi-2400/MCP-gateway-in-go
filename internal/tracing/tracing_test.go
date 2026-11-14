package tracing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// Client trace context from the W3C Trace Context spec's own example.
const (
	clientTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	clientSpanID      = "00f067aa0ba902b7"
	clientTraceParent = "00-" + clientTraceID + "-" + clientSpanID + "-01"
)

// fakeNext answers with resp and lets a test act inside the call, the way
// the policy and rate limiter write to the note.
type fakeNext struct {
	resp   *jsonrpc.Message
	err    error
	during func(ctx context.Context)
	calls  int
}

func (f *fakeNext) Call(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
	f.calls++
	if f.during != nil {
		f.during(ctx)
	}
	return f.resp, f.err
}

func (f *fakeNext) Notify(context.Context, string, json.RawMessage) error { return nil }

func okResult() *jsonrpc.Message {
	return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","content":[]}`)}
}

// traced runs one request through a Layer and returns its only span.
func traced(t *testing.T, next *fakeNext, method, params string) sdktrace.ReadOnlySpan {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	known := map[string]bool{"alpha__add": true}
	l := NewLayer(next, tp.Tracer("test"), func(tool string) bool { return known[tool] })

	ctx := auth.WithAgent(context.Background(), auth.Agent{Name: "tester"})
	if _, err := l.Call(ctx, method, json.RawMessage(params)); err != nil && next.err == nil {
		t.Fatalf("Call() error = %v", err)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]string {
	m := map[attribute.Key]string{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value.Emit()
	}
	return m
}

func callParams(tool, extraMeta string) string {
	m := meta
	if extraMeta != "" {
		m = strings.TrimSuffix(meta, "}") + "," + extraMeta + "}"
	}
	return `{"name":"` + tool + `","arguments":{"secret":"hunter2"},` + m + `}`
}

func TestAllowedCallSpan(t *testing.T) {
	next := &fakeNext{resp: okResult(), during: func(ctx context.Context) {
		note.Allow(ctx, `role "r" allows alpha__add`, "r: alpha__add")
	}}
	s := traced(t, next, "tools/call", callParams("alpha__add", ""))

	if s.Name() != "tools/call alpha__add" || s.SpanKind() != trace.SpanKindServer {
		t.Errorf("span %q kind %v, want \"tools/call alpha__add\" server", s.Name(), s.SpanKind())
	}
	want := map[attribute.Key]string{
		"mcp.method.name":       "tools/call",
		"gen_ai.operation.name": "execute_tool",
		"gen_ai.tool.name":      "alpha__add",
		"mcp.protocol.version":  "2026-07-28",
		"mcpgw.agent":           "tester",
		"mcpgw.decision":        "allowed",
		"mcpgw.rule":            "r: alpha__add",
	}
	got := attrs(s)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["error.type"]; ok || s.Status().Code == codes.Error {
		t.Errorf("allowed call has error.type %q, status %v", got["error.type"], s.Status())
	}
	if s.Parent().IsValid() {
		t.Error("span has a parent; every request should start a trace")
	}
}

func TestArgumentsNeverReachTheSpan(t *testing.T) {
	s := traced(t, &fakeNext{resp: okResult()}, "tools/call", callParams("alpha__add", ""))
	for k, v := range attrs(s) {
		if strings.Contains(v, "hunter2") {
			t.Errorf("attribute %s holds an argument value: %q", k, v)
		}
	}
}

func TestRefusedCallsAreErrors(t *testing.T) {
	tests := []struct {
		name          string
		tool          string
		resp          *jsonrpc.Message
		during        func(context.Context)
		wantName      string
		wantErrorType string
		wantDecision  string
	}{
		{
			name:          "unknown tool",
			tool:          "alpha__made_up",
			resp:          &jsonrpc.Message{Error: &jsonrpc.Error{Code: -32602, Message: "Unknown tool: alpha__made_up"}},
			during:        func(ctx context.Context) { note.Deny(ctx, "no rule allows it") },
			wantName:      "tools/call", // made-up names stay out of span names
			wantErrorType: "-32602",
			wantDecision:  "denied",
		},
		{
			name:          "rate limited",
			tool:          "alpha__add",
			resp:          &jsonrpc.Message{Error: &jsonrpc.Error{Code: -31029, Message: "rate limited"}},
			during:        func(ctx context.Context) { note.Limit(ctx, "rate limit 1/hour, burst 1", "alpha__add") },
			wantName:      "tools/call alpha__add",
			wantErrorType: "-31029",
			wantDecision:  "rate_limited",
		},
		{
			name:          "bad arguments",
			tool:          "alpha__add",
			resp:          &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","isError":true,"content":[]}`)},
			during:        func(ctx context.Context) { note.Deny(ctx, "argument a: too long") },
			wantName:      "tools/call alpha__add",
			wantErrorType: "tool_error",
			wantDecision:  "denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := traced(t, &fakeNext{resp: tt.resp, during: tt.during}, "tools/call", callParams(tt.tool, ""))
			got := attrs(s)
			if s.Name() != tt.wantName {
				t.Errorf("name = %q, want %q", s.Name(), tt.wantName)
			}
			if got["gen_ai.tool.name"] != tt.tool {
				t.Errorf("gen_ai.tool.name = %q, want %q", got["gen_ai.tool.name"], tt.tool)
			}
			if got["error.type"] != tt.wantErrorType || got["mcpgw.decision"] != tt.wantDecision {
				t.Errorf("error.type %q decision %q, want %q and %q", got["error.type"], got["mcpgw.decision"], tt.wantErrorType, tt.wantDecision)
			}
			if s.Status().Code != codes.Error {
				t.Errorf("status = %v, want Error", s.Status())
			}
		})
	}
}

func TestCancelledCall(t *testing.T) {
	s := traced(t, &fakeNext{err: context.Canceled}, "tools/call", callParams("alpha__add", ""))
	if got := attrs(s)["error.type"]; got != "cancelled" || s.Status().Code != codes.Error {
		t.Errorf("error.type = %q status %v, want cancelled and Error", got, s.Status())
	}
}

func TestClientTraceBecomesALinkOnly(t *testing.T) {
	s := traced(t, &fakeNext{resp: okResult()}, "tools/call", callParams("alpha__add", `"traceparent":"`+clientTraceParent+`","tracestate":"vendor=x"`))

	if s.SpanContext().TraceID().String() == clientTraceID {
		t.Error("span joined the client's trace; it should start its own")
	}
	if s.Parent().IsValid() {
		t.Errorf("span has parent %v; the client's context must not become the parent", s.Parent())
	}
	links := s.Links()
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	sc := links[0].SpanContext
	if sc.TraceID().String() != clientTraceID || sc.SpanID().String() != clientSpanID {
		t.Errorf("link to %s/%s, want %s/%s", sc.TraceID(), sc.SpanID(), clientTraceID, clientSpanID)
	}
	if sc.TraceState().Get("vendor") != "x" {
		t.Errorf("link tracestate = %q, want vendor=x", sc.TraceState().String())
	}
}

func TestBadClientTraceGivesNoLink(t *testing.T) {
	for _, extra := range []string{
		`"traceparent":"not-a-trace"`,
		`"traceparent":"00-00000000000000000000000000000000-00f067aa0ba902b7-01"`, // all-zero trace ID is invalid
		`"traceparent":12`,
	} {
		s := traced(t, &fakeNext{resp: okResult()}, "tools/call", callParams("alpha__add", extra))
		if n := len(s.Links()); n != 0 {
			t.Errorf("%s: got %d links, want 0", extra, n)
		}
	}
}

func TestOtherMethods(t *testing.T) {
	tests := []struct{ method, wantName string }{
		{"tools/list", "tools/list"},
		{"server/discover", "server/discover"},
		{"made/up", "_OTHER"},
	}
	for _, tt := range tests {
		s := traced(t, &fakeNext{resp: okResult()}, tt.method, `{`+meta+`}`)
		got := attrs(s)
		if s.Name() != tt.wantName || got["mcp.method.name"] != tt.wantName {
			t.Errorf("%s: name %q, mcp.method.name %q, want %q", tt.method, s.Name(), got["mcp.method.name"], tt.wantName)
		}
		if _, ok := got["gen_ai.tool.name"]; ok {
			t.Errorf("%s: has gen_ai.tool.name", tt.method)
		}
		if _, ok := got["mcpgw.decision"]; ok {
			t.Errorf("%s: has a decision; only tools/call is decided", tt.method)
		}
	}
}

func TestLongToolNameIsCapped(t *testing.T) {
	long := strings.Repeat("é", 300) // 600 bytes
	s := traced(t, &fakeNext{resp: okResult()}, "tools/call", callParams(long, ""))
	got := attrs(s)["gen_ai.tool.name"]
	if len(got) > maxAttrLen || !strings.HasPrefix(long, got) || got == "" {
		t.Errorf("tool attribute is %d bytes, want at most %d and a whole-character prefix", len(got), maxAttrLen)
	}
}

func TestNotifyHasNoSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	l := NewLayer(&fakeNext{}, tp.Tracer("test"), func(string) bool { return true })
	if err := l.Notify(context.Background(), "notifications/cancelled", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("Notify made %d spans, want 0", n)
	}
}

// Even with a span already in the context (say, from HTTP instrumentation
// added later), each request starts its own trace.
func TestSpanInContextIsNotTheParent(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	l := NewLayer(&fakeNext{resp: okResult()}, tp.Tracer("test"), func(string) bool { return true })

	ctx, outer := tp.Tracer("test").Start(context.Background(), "outer")
	if _, err := l.Call(ctx, "tools/call", json.RawMessage(callParams("alpha__add", ""))); err != nil {
		t.Fatal(err)
	}
	outer.End()
	for _, s := range sr.Ended() {
		if s.Name() != "outer" && s.Parent().IsValid() {
			t.Errorf("span %q has parent %v, want a new trace", s.Name(), s.Parent().SpanID())
		}
	}
}
