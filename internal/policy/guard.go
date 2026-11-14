package policy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// Handler answers client requests. It has the same shape as proxy.Handler,
// repeated here so the policy package does not depend on the proxy. The
// Guard both wraps a Handler and is one.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
	Notify(ctx context.Context, method string, params json.RawMessage) error
}

// SchemaSource looks up a tool's inputSchema, like router.Router.ToolSchema.
type SchemaSource interface {
	ToolSchema(ctx context.Context, tool string, meta json.RawMessage) (schema json.RawMessage, found bool, err error)
}

// Guard enforces a Policy in front of the router. It reads the agent from
// the request context (auth.WithAgent), so stdio and HTTP are checked the
// same way. Anything it cannot check is denied.
type Guard struct {
	next    Handler
	schemas SchemaSource
	policy  *Policy
	logger  *slog.Logger
}

// NewGuard returns a Guard that passes allowed requests to next. schemas
// may be nil when nothing calls ToolSchema (the stdio transport).
func NewGuard(next Handler, schemas SchemaSource, p *Policy, logger *slog.Logger) *Guard {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Guard{next: next, schemas: schemas, policy: p, logger: logger}
}

// policyCheckFailed is the only thing a client learns when the guard itself
// fails. Details go to the log.
func policyCheckFailed() *jsonrpc.Message {
	return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "policy check failed"}}
}

// Call checks one request and passes it on if allowed. Only the methods the
// guard knows are passed on. If the router later learns a new method, like
// resources/read, it stays unreachable until the guard has a rule for it.
func (g *Guard) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	agent, ok := auth.FromContext(ctx)
	if !ok {
		// Every transport authenticates before calling the guard, so this is
		// a bug. Fail closed.
		g.logger.Error("request has no agent; denying", "method", method)
		note.Deny(ctx, "request has no agent")
		return policyCheckFailed(), nil
	}
	switch method {
	case "server/discover":
		return g.next.Call(ctx, method, params)
	case "tools/list":
		return g.listTools(ctx, agent.Name, params)
	case "tools/call":
		return g.callTool(ctx, agent.Name, params)
	default:
		g.logger.Info("method denied", "agent", agent.Name, "method", method)
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found: " + method}}, nil
	}
}

// Notify passes notifications on. None of them call tools.
func (g *Guard) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return g.next.Notify(ctx, method, params)
}

// listTools asks the router for the merged list and keeps only the tools
// the agent may call. The spec allows tools/list to vary with the caller's
// credentials. If the list can't be read, the agent gets an error and never
// the unfiltered list.
func (g *Guard) listTools(ctx context.Context, agent string, params json.RawMessage) (*jsonrpc.Message, error) {
	resp, err := g.next.Call(ctx, "tools/list", params)
	if err != nil || resp == nil || resp.Error != nil {
		return resp, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &fields); err != nil {
		g.logger.Error("can't read tools/list result to filter it", "error", err)
		return policyCheckFailed(), nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(fields["tools"], &tools); err != nil {
		g.logger.Error("can't read tools in tools/list result to filter them", "error", err)
		return policyCheckFailed(), nil
	}

	kept := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		// A tool whose name can't be read can't be checked, so it is left out.
		if err := json.Unmarshal(raw, &tool); err != nil {
			continue
		}
		if g.policy.AllowTool(agent, tool.Name).Allowed {
			kept = append(kept, raw)
		}
	}
	g.logger.Debug("filtered tools/list", "agent", agent, "kept", len(kept), "hidden", len(tools)-len(kept))

	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	fields["tools"] = encoded
	result, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return &jsonrpc.Message{Result: result}, nil
}

// callTool checks a tools/call. A tool the agent may not call gets the
// same answer as a tool that does not exist, so agents can't probe for
// hidden tools. Arguments that fail a check get a tool error result, which
// clients show the model so it can try again with different arguments.
func (g *Guard) callTool(ctx context.Context, agent string, params json.RawMessage) (*jsonrpc.Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		note.Deny(ctx, "invalid tools/call params")
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tools/call params"}}, nil
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil || name == "" {
		note.Deny(ctx, "tools/call has no tool name")
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tools/call needs a tool name"}}, nil
	}

	d := g.policy.AllowCall(agent, name, fields["arguments"])
	if d.Allowed {
		g.logger.Debug("call allowed", "agent", agent, "tool", name, "rule", d.Rule)
		note.Allow(ctx, d.Reason, d.Rule)
		return g.next.Call(ctx, "tools/call", params)
	}
	g.logger.Info("call denied", "agent", agent, "tool", name, "reason", d.Reason)
	note.Deny(ctx, d.Reason)
	if d.ArgProblem == "" {
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Unknown tool: " + name}}, nil
	}
	return deniedResult("Denied by gateway policy: " + d.ArgProblem)
}

func deniedResult(text string) (*jsonrpc.Message, error) {
	type textContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	result, err := json.Marshal(struct {
		ResultType string        `json:"resultType"`
		Content    []textContent `json:"content"`
		IsError    bool          `json:"isError"`
	}{"complete", []textContent{{"text", text}}, true})
	if err != nil {
		return nil, err
	}
	return &jsonrpc.Message{Result: result}, nil
}

// ToolSchema passes schema lookups on only for tools the agent may call.
// The HTTP transport looks up a schema before forwarding a call. Without
// this check, a call to a hidden tool would make the gateway list that
// upstream, and the different error would show the tool exists.
func (g *Guard) ToolSchema(ctx context.Context, tool string, meta json.RawMessage) (json.RawMessage, bool, error) {
	agent, ok := auth.FromContext(ctx)
	if !ok || !g.policy.AllowTool(agent.Name, tool).Allowed {
		return nil, false, nil
	}
	if g.schemas == nil {
		return nil, false, errors.New("policy: guard has no schema source")
	}
	return g.schemas.ToolSchema(ctx, tool, meta)
}
