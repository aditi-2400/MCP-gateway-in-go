package tracing

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// recordingCaller keeps the params the upstream was sent.
type recordingCaller struct {
	resp *jsonrpc.Message
	sent json.RawMessage
}

func (c *recordingCaller) Call(_ context.Context, _ string, params json.RawMessage) (*jsonrpc.Message, error) {
	c.sent = params
	return c.resp, nil
}

// sentMeta decodes the _meta the upstream received.
func sentMeta(t *testing.T, c *recordingCaller) map[string]json.RawMessage {
	t.Helper()
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(c.sent, &p); err != nil {
		t.Fatalf("upstream got unreadable params %s: %v", c.sent, err)
	}
	return p.Meta
}

const clientTraceMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","progressToken":"t1",` +
	`"traceparent":"` + clientTraceParent + `","tracestate":"vendor=x","baggage":"user=alice"}`

func knownAdd(name string) bool { return name == "alpha__add" }

func TestUpstreamSpanIsAChildAndForwardsItsOwnContext(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	caller := &recordingCaller{resp: okResult()}
	up := WrapUpstream("alpha", caller, tp.Tracer("test"), knownAdd)

	ctx, server := tp.Tracer("test").Start(context.Background(), "tools/call alpha__add", trace.WithSpanKind(trace.SpanKindServer))
	_, err := up.Call(ctx, "tools/call", json.RawMessage(`{"name":"add","arguments":{"a":1},`+clientTraceMeta+`}`))
	server.End()
	if err != nil {
		t.Fatal(err)
	}

	var client sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			client = s
		}
	}
	if client == nil {
		t.Fatal("no CLIENT span")
	}
	if client.Name() != "tools/call add" {
		t.Errorf("name = %q, want \"tools/call add\"", client.Name())
	}
	if client.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Error("CLIENT span is not a child of the request's SERVER span")
	}
	got := attrs(client)
	if got["mcpgw.upstream"] != "alpha" || got["gen_ai.tool.name"] != "add" || got["mcp.method.name"] != "tools/call" {
		t.Errorf("attributes = %v", got)
	}

	meta := sentMeta(t, caller)
	var traceparent string
	if err := json.Unmarshal(meta["traceparent"], &traceparent); err != nil {
		t.Fatalf("forwarded traceparent %s: %v", meta["traceparent"], err)
	}
	want := "00-" + client.SpanContext().TraceID().String() + "-" + client.SpanContext().SpanID().String() + "-01"
	if traceparent != want {
		t.Errorf("forwarded traceparent = %s, want the CLIENT span's %s", traceparent, want)
	}
	for _, k := range []string{"tracestate", "baggage"} {
		if v, ok := meta[k]; ok {
			t.Errorf("client's %s %s reached the upstream", k, v)
		}
	}
	// Everything else in _meta is untouched.
	if string(meta["progressToken"]) != `"t1"` || string(meta["io.modelcontextprotocol/protocolVersion"]) != `"2026-07-28"` {
		t.Errorf("other _meta fields changed: %v", meta)
	}
}

func TestNoopTracerStillRemovesClientTraceKeys(t *testing.T) {
	caller := &recordingCaller{resp: okResult()}
	up := WrapUpstream("alpha", caller, noop.NewTracerProvider().Tracer(""), knownAdd)
	if _, err := up.Call(context.Background(), "tools/call", json.RawMessage(`{"name":"add",`+clientTraceMeta+`}`)); err != nil {
		t.Fatal(err)
	}
	meta := sentMeta(t, caller)
	for _, k := range traceKeys {
		if v, ok := meta[k]; ok {
			t.Errorf("%s %s reached the upstream with tracing off", k, v)
		}
	}
	if string(meta["progressToken"]) != `"t1"` {
		t.Errorf("progressToken lost: %v", meta)
	}
}

func TestParamsWithoutTraceKeysAreForwardedAsIs(t *testing.T) {
	caller := &recordingCaller{resp: okResult()}
	up := WrapUpstream("alpha", caller, noop.NewTracerProvider().Tracer(""), knownAdd)
	for _, params := range []string{
		`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`,
		`{"name":"add","arguments":{"b":2,"a":1}}`,
		`{"name":"add","_meta":null}`,
	} {
		if _, err := up.Call(context.Background(), "tools/list", json.RawMessage(params)); err != nil {
			t.Fatal(err)
		}
		if string(caller.sent) != params {
			t.Errorf("sent %s, want %s unchanged", caller.sent, params)
		}
	}
}

func TestNullMetaGetsTheGatewayContext(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	caller := &recordingCaller{resp: okResult()}
	up := WrapUpstream("alpha", caller, tp.Tracer("test"), knownAdd)
	if _, err := up.Call(context.Background(), "tools/list", json.RawMessage(`{"_meta":null}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := sentMeta(t, caller)["traceparent"]; !ok {
		t.Errorf("sent %s, want a traceparent", caller.sent)
	}
}

func TestUpstreamSpanBoundsItsName(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	caller := &recordingCaller{resp: &jsonrpc.Message{Error: &jsonrpc.Error{Code: -32602, Message: "Unknown tool: made_up"}}}
	up := WrapUpstream("alpha", caller, tp.Tracer("test"), knownAdd)
	if _, err := up.Call(context.Background(), "tools/call", json.RawMessage(`{"name":"made_up"}`)); err != nil {
		t.Fatal(err)
	}
	s := sr.Ended()[0]
	if s.Name() != "tools/call" {
		t.Errorf("name = %q, want \"tools/call\" for a tool no upstream listed", s.Name())
	}
	if attrs(s)["error.type"] != "-32602" || s.Status().Code != codes.Error {
		t.Errorf("error.type %q status %v, want -32602 and Error", attrs(s)["error.type"], s.Status())
	}
}

func TestBadParamsAreNotForwarded(t *testing.T) {
	caller := &recordingCaller{resp: okResult()}
	up := WrapUpstream("alpha", caller, noop.NewTracerProvider().Tracer(""), knownAdd)
	for _, params := range []string{`[1,2]`, `{"_meta":"text"}`} {
		if _, err := up.Call(context.Background(), "tools/list", json.RawMessage(params)); err == nil {
			t.Errorf("%s: no error", params)
		}
	}
	if caller.sent != nil {
		t.Errorf("upstream was sent %s", caller.sent)
	}
}
