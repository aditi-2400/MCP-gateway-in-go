package ratelimit

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

type countingRouter struct {
	mu    sync.Mutex
	calls []string
}

func (r *countingRouter) Call(_ context.Context, method string, _ json.RawMessage) (*jsonrpc.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method)
	return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete"}`)}, nil
}

func (r *countingRouter) Notify(context.Context, string, json.RawMessage) error { return nil }

func (r *countingRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// fixedLimits gives email__send a tight limit and everything else a loose one.
type fixedLimits struct{}

func (fixedLimits) LimitFor(tool string) (Limit, string) {
	if tool == "email__send" {
		return Limit{Count: 5, Per: time.Minute, Burst: 2}, "email__send"
	}
	return Limit{Count: 100, Per: time.Second, Burst: 100}, "default"
}

func newTestEnforcer() (*Enforcer, *countingRouter, *fakeClock) {
	clock := newFakeClock()
	router := &countingRouter{}
	return NewEnforcer(router, fixedLimits{}, New(clock.now), nil), router, clock
}

func agentCtx(name string) context.Context {
	return auth.WithAgent(context.Background(), auth.Agent{Name: name})
}

func call(e *Enforcer, ctx context.Context, tool string) *jsonrpc.Message {
	resp, err := e.Call(ctx, "tools/call", json.RawMessage(`{"name":"`+tool+`"}`))
	if err != nil {
		panic(err)
	}
	return resp
}

func TestEnforcerBurstThenLimit(t *testing.T) {
	e, router, clock := newTestEnforcer()
	ctx := agentCtx("bot")
	for i := range 2 {
		if resp := call(e, ctx, "email__send"); resp.Error != nil {
			t.Fatalf("call %d: %v, want allowed (burst 2)", i, resp.Error)
		}
	}
	resp := call(e, ctx, "email__send")
	if resp.Error == nil || resp.Error.Code != CodeRateLimited || resp.Error.Message != "Rate limit exceeded for tool email__send" {
		t.Fatalf("third call = %+v, want a -31029 rate limit error", resp)
	}
	if string(resp.Error.Data) != `{"retryAfterMs":12000}` {
		t.Errorf("data = %s, want retryAfterMs 12000 (5/minute is one call every 12s)", resp.Error.Data)
	}
	if router.count() != 2 {
		t.Errorf("router saw %d calls, want 2: a limited call must not reach the upstream", router.count())
	}

	// Other tools and other agents have their own buckets.
	if resp := call(e, ctx, "fs__read"); resp.Error != nil {
		t.Errorf("other tool limited: %v", resp.Error)
	}
	if resp := call(e, agentCtx("other"), "email__send"); resp.Error != nil {
		t.Errorf("other agent limited: %v", resp.Error)
	}

	clock.advance(12 * time.Second)
	if resp := call(e, ctx, "email__send"); resp.Error != nil {
		t.Errorf("after 12s: %v, want allowed", resp.Error)
	}
}

func TestEnforcerRoundsRetryUp(t *testing.T) {
	e, _, clock := newTestEnforcer()
	ctx := agentCtx("bot")
	call(e, ctx, "email__send")
	call(e, ctx, "email__send")
	clock.advance(time.Second + 300*time.Microsecond) // 10.9997s left
	resp := call(e, ctx, "email__send")
	if string(resp.Error.Data) != `{"retryAfterMs":11000}` {
		t.Errorf("data = %s, want 11000: rounding down would send the client back too early", resp.Error.Data)
	}
}

func TestEnforcerOnlyCountsToolCalls(t *testing.T) {
	e, router, _ := newTestEnforcer()
	ctx := agentCtx("bot")
	for range 10 {
		if resp, err := e.Call(ctx, "tools/list", json.RawMessage(`{}`)); err != nil || resp.Error != nil {
			t.Fatalf("tools/list = %+v, %v", resp, err)
		}
	}
	if resp := call(e, ctx, "email__send"); resp.Error != nil {
		t.Fatalf("tools/call after lists: %v, want the full burst still there", resp.Error)
	}
	if router.count() != 11 {
		t.Errorf("router saw %d calls, want 11", router.count())
	}
}

func TestEnforcerFailsClosed(t *testing.T) {
	e, router, _ := newTestEnforcer()
	resp := call(e, context.Background(), "fs__read")
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError {
		t.Errorf("no agent = %+v, want -32603", resp)
	}
	for _, params := range []string{`[1]`, `{}`, `{"name":7}`} {
		resp, err := e.Call(agentCtx("bot"), "tools/call", json.RawMessage(params))
		if err != nil || resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("params %s = %+v, %v; want -32602", params, resp, err)
		}
	}
	if router.count() != 0 {
		t.Errorf("router saw %d calls, want none", router.count())
	}
}

func TestRetryAfter(t *testing.T) {
	limited, _ := rateLimited("x__y", 1500*time.Millisecond)
	if wait, ok := RetryAfter(limited.Error); !ok || wait != 1500*time.Millisecond {
		t.Errorf("RetryAfter = %v, %v; want 1.5s", wait, ok)
	}
	for _, e := range []*jsonrpc.Error{
		nil,
		{Code: jsonrpc.CodeInternalError, Message: "x"},
		{Code: CodeRateLimited, Message: "x"},
		{Code: CodeRateLimited, Message: "x", Data: json.RawMessage(`{"retryAfterMs":-5}`)},
		{Code: CodeRateLimited, Message: "x", Data: json.RawMessage(`"soon"`)},
	} {
		if _, ok := RetryAfter(e); ok {
			t.Errorf("RetryAfter(%+v) ok, want not ok", e)
		}
	}
}

func TestCodeOutsideReservedRange(t *testing.T) {
	// The spec asks for implementation codes outside -32768..-32000.
	if CodeRateLimited >= -32768 && CodeRateLimited <= -32000 {
		t.Fatalf("CodeRateLimited %d is inside the reserved range", CodeRateLimited)
	}
	if !strings.HasPrefix(jsonrpcErrorJSON(t), `{"code":-31029,`) {
		t.Errorf("error JSON = %s", jsonrpcErrorJSON(t))
	}
}

func jsonrpcErrorJSON(t *testing.T) string {
	t.Helper()
	msg, _ := rateLimited("x__y", time.Second)
	data, err := json.Marshal(msg.Error)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEnforcerNotesLimits(t *testing.T) {
	e, _, _ := newTestEnforcer()
	ctx, n := note.New(agentCtx("bot"))
	call(e, ctx, "email__send")
	if got := n.Get(); got.Decision != "" {
		t.Fatalf("allowed call noted %+v; the limiter only notes refusals", got)
	}
	call(e, ctx, "email__send")
	call(e, ctx, "email__send")
	got := n.Get()
	if got.Decision != note.RateLimited || got.Rule != "email__send" || !strings.Contains(got.Reason, "5/minute, burst 2") || !strings.Contains(got.Reason, "retry in 12s") {
		t.Errorf("note = %+v, want rate_limited by email__send with the limit and wait", got)
	}
}
