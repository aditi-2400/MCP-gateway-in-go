package tracing

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const attrUpstream = attribute.Key("mcpgw.upstream")

// separator joins an upstream name and a tool name, as router.Separator.
const separator = "__"

// traceKeys are the _meta keys MCP reserves for trace context.
var traceKeys = []string{"traceparent", "tracestate", "baggage"}

// Caller is one upstream connection, like router.Upstream.
type Caller interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
}

// Upstream wraps one upstream connection. Each call gets a CLIENT span,
// and the trace context in the forwarded _meta is the gateway's own: the
// client's traceparent, tracestate, and baggage are removed, so nothing a
// client puts there crosses to the upstream, and the gateway's traceparent
// is added when the span is being recorded. With a no-op tracer it only
// removes.
type Upstream struct {
	name      string
	next      Caller
	tracer    trace.Tracer
	knownTool func(string) bool
}

// WrapUpstream returns next with tracing. name is the upstream's name, the
// prefix of its tools. knownTool takes prefixed names (router.KnownTool).
func WrapUpstream(name string, next Caller, tracer trace.Tracer, knownTool func(string) bool) *Upstream {
	return &Upstream{name: name, next: next, tracer: tracer, knownTool: knownTool}
}

// Call traces one upstream call and forwards it with the gateway's trace
// context.
func (u *Upstream) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	var tool string
	isCall := method == "tools/call"
	if isCall {
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("tracing: read %s params: %w", method, err)
		}
		tool = p.Name
	}

	attrs := []attribute.KeyValue{attrMethod.String(spanMethod(method)), attrUpstream.String(u.name)}
	name := spanMethod(method)
	if isCall {
		attrs = append(attrs, attrOperation.String("execute_tool"), attrTool.String(capped(tool)))
		if tool != "" && u.knownTool(u.name+separator+tool) {
			name += " " + tool
		}
	}
	ctx, span := u.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()

	forwarded, err := withTraceContext(ctx, params)
	if err != nil {
		recordOutcome(span, nil, err)
		return nil, err
	}
	resp, err := u.next.Call(ctx, method, forwarded)
	recordOutcome(span, resp, err)
	return resp, err
}

// withTraceContext replaces the trace keys in params._meta with the trace
// context in ctx. Every other field is kept as it was. params comes from
// the router, which always builds an object.
func withTraceContext(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		return nil, fmt.Errorf("tracing: params are not an object: %w", err)
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := fields["_meta"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("tracing: _meta is not an object: %w", err)
		}
		if meta == nil { // "_meta": null
			meta = map[string]json.RawMessage{}
		}
	}

	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	removed := 0
	for _, k := range traceKeys {
		if _, ok := meta[k]; ok {
			delete(meta, k)
			removed++
		}
	}
	if removed == 0 && len(carrier) == 0 {
		return params, nil // nothing to change
	}
	for k, v := range carrier {
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("tracing: encode %s: %w", k, err)
		}
		meta[k] = encoded
	}

	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("tracing: encode _meta: %w", err)
	}
	fields["_meta"] = encodedMeta
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("tracing: encode params: %w", err)
	}
	return out, nil
}
