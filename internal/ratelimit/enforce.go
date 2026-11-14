package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// CodeRateLimited is the JSON-RPC error code for a rate-limited call. The
// spec reserves -32768 to -32000 and asks implementations to put their own
// codes outside it, so this one sits just past the end.
const CodeRateLimited = -31029

// Handler answers client requests, with the same shape as proxy.Handler.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
	Notify(ctx context.Context, method string, params json.RawMessage) error
}

// LimitSource says which limit applies to a tool, and which config entry it
// came from (for logs). policy.Policy is one.
type LimitSource interface {
	LimitFor(tool string) (Limit, string)
}

// Enforcer counts tools/call requests against each agent's bucket for the
// tool, and refuses calls when the bucket is empty. It sits behind the
// policy guard, so calls the policy denies never use up tokens.
type Enforcer struct {
	next    Handler
	limits  LimitSource
	limiter *Limiter
	logger  *slog.Logger
}

// NewEnforcer returns an Enforcer that passes calls within the limits to next.
func NewEnforcer(next Handler, limits LimitSource, limiter *Limiter, logger *slog.Logger) *Enforcer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Enforcer{next: next, limits: limits, limiter: limiter, logger: logger}
}

type retryData struct {
	RetryAfterMs int64 `json:"retryAfterMs"`
}

// Call checks tools/call against the limit and passes everything else on.
func (e *Enforcer) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	if method != "tools/call" {
		return e.next.Call(ctx, method, params)
	}
	agent, ok := auth.FromContext(ctx)
	if !ok {
		// The transports always attach an agent, so this is a bug. Without
		// an agent there is no bucket to count against: fail closed.
		e.logger.Error("tools/call has no agent; denying")
		note.Deny(ctx, "request has no agent")
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "rate limit check failed"}}, nil
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tools/call needs a tool name"}}, nil
	}

	lim, entry := e.limits.LimitFor(p.Name)
	allowed, retry := e.limiter.Allow(agent.Name, p.Name, lim)
	if allowed {
		return e.next.Call(ctx, method, params)
	}
	e.logger.Info("call rate limited", "agent", agent.Name, "tool", p.Name, "limit", lim.String(), "entry", entry, "retry_after", retry)
	note.Limit(ctx, fmt.Sprintf("rate limit %s; retry in %v", lim, retry), entry)
	return rateLimited(p.Name, retry)
}

// Notify passes notifications on; they are not counted.
func (e *Enforcer) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return e.next.Notify(ctx, method, params)
}

func rateLimited(tool string, retry time.Duration) (*jsonrpc.Message, error) {
	// Round up, so a client that waits exactly this long finds a token.
	ms := (retry + time.Millisecond - 1) / time.Millisecond
	data, err := json.Marshal(retryData{RetryAfterMs: int64(ms)})
	if err != nil {
		return nil, err
	}
	return &jsonrpc.Message{Error: &jsonrpc.Error{
		Code:    CodeRateLimited,
		Message: "Rate limit exceeded for tool " + tool,
		Data:    data,
	}}, nil
}

// RetryAfter reads the wait from a rate-limit error, for transports that
// can pass it on (HTTP's Retry-After header). ok is false for other errors.
func RetryAfter(e *jsonrpc.Error) (wait time.Duration, ok bool) {
	if e == nil || e.Code != CodeRateLimited {
		return 0, false
	}
	var d retryData
	if err := json.Unmarshal(e.Data, &d); err != nil || d.RetryAfterMs < 0 {
		return 0, false
	}
	return time.Duration(d.RetryAfterMs) * time.Millisecond, true
}
