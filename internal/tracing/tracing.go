// Package tracing records an OpenTelemetry span for every client request.
//
// Names and attributes follow the OpenTelemetry conventions for MCP
// (docs/gen-ai/mcp.md in open-telemetry/semantic-conventions-genai, still in
// Development): spans are named "{mcp.method.name} {tool}", and attributes
// include mcp.method.name, gen_ai.tool.name, and error.type. Gateway-only
// attributes use the mcpgw. prefix.
//
// Every request starts a new trace. A client may send its own trace context
// in params._meta (traceparent, tracestate), but that is untrusted input: a
// client could pick trace IDs, force sampling, or join someone else's trace.
// So the client's context only becomes a span link, which trace viewers can
// follow, and never the parent.
package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// Attribute keys. The first five come from the OpenTelemetry conventions.
const (
	attrMethod    = attribute.Key("mcp.method.name")
	attrOperation = attribute.Key("gen_ai.operation.name")
	attrTool      = attribute.Key("gen_ai.tool.name")
	attrProtocol  = attribute.Key("mcp.protocol.version")
	attrErrorType = attribute.Key("error.type")
	attrAgent     = attribute.Key("mcpgw.agent")
	attrDecision  = attribute.Key("mcpgw.decision")
	attrReason    = attribute.Key("mcpgw.reason")
	attrRule      = attribute.Key("mcpgw.rule")
)

// other stands in for values outside a known set: methods the gateway
// doesn't serve, so a client can't fill the trace backend with made-up span
// names, and errors with no better name. The OpenTelemetry conventions use
// the same word for unknown HTTP methods and error types.
const other = "_OTHER"

// maxAttrLen caps attribute values that come from the client.
const maxAttrLen = 256

// knownMethods are the methods that may appear in span names.
var knownMethods = map[string]bool{
	"server/discover": true,
	"tools/list":      true,
	"tools/call":      true,
}

// Handler answers client requests, with the same shape as proxy.Handler.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
	Notify(ctx context.Context, method string, params json.RawMessage) error
}

// Layer starts a SERVER span for each request and passes it on. It goes in
// front of the audit layer, so the span covers the whole time in the
// gateway, including a wait for room in the audit queue.
type Layer struct {
	next      Handler
	tracer    trace.Tracer
	knownTool func(string) bool
}

// NewLayer returns a Layer in front of next. knownTool says whether a tool
// name is real (router.KnownTool); only real names go in span names.
func NewLayer(next Handler, tracer trace.Tracer, knownTool func(string) bool) *Layer {
	return &Layer{next: next, tracer: tracer, knownTool: knownTool}
}

// request is the part of params the span needs.
type request struct {
	Name string `json:"name"`
	Meta struct {
		Protocol    string `json:"io.modelcontextprotocol/protocolVersion"`
		TraceParent string `json:"traceparent"`
		TraceState  string `json:"tracestate"`
	} `json:"_meta"`
}

// Call traces one request.
func (l *Layer) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	var req request
	if err := json.Unmarshal(params, &req); err != nil {
		// The router and policy answer malformed params. The span just
		// carries less.
		req = request{}
	}
	isCall := method == "tools/call"

	attrs := []attribute.KeyValue{attrMethod.String(spanMethod(method))}
	if p := req.Meta.Protocol; p != "" {
		attrs = append(attrs, attrProtocol.String(capped(p)))
	}
	if agent, ok := auth.FromContext(ctx); ok {
		attrs = append(attrs, attrAgent.String(agent.Name))
	}
	if isCall {
		attrs = append(attrs, attrOperation.String("execute_tool"), attrTool.String(capped(req.Name)))
	}
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	}
	if link, ok := clientLink(req); ok {
		opts = append(opts, trace.WithLinks(link))
	}

	ctx, span := l.tracer.Start(ctx, l.spanName(method, req.Name, isCall), opts...)
	defer span.End()

	var n *note.Note
	if isCall {
		ctx, n = note.Ensure(ctx)
	}
	resp, err := l.next.Call(ctx, method, params)
	if n != nil {
		recordDecision(span, n.Get())
	}
	recordOutcome(span, resp, err)
	return resp, err
}

// Notify passes notifications on without a span.
func (l *Layer) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return l.next.Notify(ctx, method, params)
}

// spanName is "tools/call {tool}" for a real tool, else just the method.
func (l *Layer) spanName(method, tool string, isCall bool) string {
	name := spanMethod(method)
	if isCall && tool != "" && l.knownTool(tool) {
		name += " " + tool
	}
	return name
}

func spanMethod(method string) string {
	if knownMethods[method] {
		return method
	}
	return other
}

// clientLink turns a client's traceparent into a span link. An invalid one
// gives no link.
func clientLink(req request) (trace.Link, bool) {
	if req.Meta.TraceParent == "" {
		return trace.Link{}, false
	}
	carrier := propagation.MapCarrier{"traceparent": req.Meta.TraceParent}
	if req.Meta.TraceState != "" {
		carrier["tracestate"] = req.Meta.TraceState
	}
	// Extracted into a context of its own, so the client's span never ends up
	// in the request context where it could become a parent.
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc}, true
}

// recordDecision copies what the policy and rate limiter decided.
func recordDecision(span trace.Span, s note.Snapshot) {
	if s.Decision == "" {
		return
	}
	span.SetAttributes(attrDecision.String(string(s.Decision)), attrReason.String(s.Reason))
	if s.Rule != "" {
		span.SetAttributes(attrRule.String(s.Rule))
	}
}

// recordOutcome sets error.type and the span status the way the conventions
// say: the JSON-RPC error code, or "tool_error" for an isError result.
func recordOutcome(span trace.Span, resp *jsonrpc.Message, err error) {
	switch {
	case err != nil:
		span.SetAttributes(attrErrorType.String(errorType(err)))
		span.SetStatus(codes.Error, err.Error())
	case resp == nil:
		// A notification-style answer; nothing to record.
	case resp.Error != nil:
		span.SetAttributes(attrErrorType.String(strconv.Itoa(resp.Error.Code)))
		span.SetStatus(codes.Error, capped(resp.Error.Message))
	case isToolError(resp.Result):
		span.SetAttributes(attrErrorType.String("tool_error"))
		span.SetStatus(codes.Error, "tool returned an error result")
	}
}

func errorType(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return other
	}
}

func isToolError(result json.RawMessage) bool {
	var r struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(result, &r) == nil && r.IsError
}

// capped cuts client-supplied text to maxAttrLen bytes without splitting a
// character.
func capped(s string) string {
	if len(s) <= maxAttrLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxAttrLen], "")
}
