package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// scriptedNext stands in for the policy guard, rate limiter, and router:
// it fills the note the way they would and returns a canned reply.
type scriptedNext struct {
	do    func(ctx context.Context) (*jsonrpc.Message, error)
	calls int
}

func (s *scriptedNext) Call(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
	s.calls++
	return s.do(ctx)
}

func (s *scriptedNext) Notify(context.Context, string, json.RawMessage) error { return nil }

func okResult() *jsonrpc.Message {
	return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","content":[]}`)}
}

func asAgent(name string) context.Context {
	return auth.WithAgent(context.Background(), auth.Agent{Name: name})
}

// runLayer sends one tools/call through a Layer and returns the entry
// written for it.
func runLayer(t *testing.T, params string, do func(ctx context.Context) (*jsonrpc.Message, error)) (Entry, *jsonrpc.Message, error) {
	t.Helper()
	store := &fakeStore{}
	rec := NewRecorder(store, Options{})
	layer := NewLayer(&scriptedNext{do: do}, rec, nil)
	resp, err := layer.Call(asAgent("ci-bot"), "tools/call", json.RawMessage(params))
	closeRecorder(t, rec)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.entries) != 1 {
		t.Fatalf("%d entries written, want 1", len(store.entries))
	}
	return store.entries[0], resp, err
}

func TestLayerRecordsOutcomes(t *testing.T) {
	const params = `{"name":"fs__write_file","arguments":{"path":"/tmp/a","password":"hunter2"}}`
	tests := []struct {
		name     string
		do       func(ctx context.Context) (*jsonrpc.Message, error)
		decision Decision
		status   Status
		code     int
		reason   string
		rule     string
		latency  time.Duration
	}{
		{
			name: "allowed and ok",
			do: func(ctx context.Context) (*jsonrpc.Message, error) {
				note.Allow(ctx, "role w allows fs__write_file", "w: fs__write_file")
				note.Upstream(ctx, 7*time.Millisecond)
				return okResult(), nil
			},
			decision: Allowed, status: StatusOK, reason: "role w allows fs__write_file", rule: "w: fs__write_file", latency: 7 * time.Millisecond,
		},
		{
			name: "upstream tool error",
			do: func(ctx context.Context) (*jsonrpc.Message, error) {
				note.Allow(ctx, "ok", "w: fs__write_file")
				note.Upstream(ctx, time.Millisecond)
				return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","isError":true,"content":[]}`)}, nil
			},
			decision: Allowed, status: StatusToolError, reason: "ok", rule: "w: fs__write_file", latency: time.Millisecond,
		},
		{
			name: "arguments denied",
			do: func(ctx context.Context) (*jsonrpc.Message, error) {
				note.Deny(ctx, `argument "path" is outside the allowed folders`)
				return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","isError":true,"content":[]}`)}, nil
			},
			decision: Denied, status: StatusToolError, reason: `argument "path" is outside the allowed folders`,
		},
		{
			name: "rate limited",
			do: func(ctx context.Context) (*jsonrpc.Message, error) {
				note.Allow(ctx, "ok", "w: fs__write_file")
				note.Limit(ctx, "rate limit 5/minute, burst 2; retry in 12s", "fs__write_file")
				return &jsonrpc.Message{Error: &jsonrpc.Error{Code: -31029, Message: "Rate limit exceeded"}}, nil
			},
			decision: RateLimited, status: StatusRPCError, code: -31029, reason: "rate limit 5/minute, burst 2; retry in 12s", rule: "fs__write_file",
		},
		{
			name: "upstream timed out",
			do: func(ctx context.Context) (*jsonrpc.Message, error) {
				note.Allow(ctx, "ok", "w: fs__write_file")
				note.Upstream(ctx, 30*time.Second)
				return nil, context.DeadlineExceeded
			},
			decision: Allowed, status: StatusNoResponse, reason: "ok", rule: "w: fs__write_file", latency: 30 * time.Second,
		},
		{
			name: "nobody decided",
			do: func(context.Context) (*jsonrpc.Message, error) {
				return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "x"}}, nil
			},
			decision: Denied, status: StatusRPCError, code: jsonrpc.CodeInternalError, reason: "stopped before a policy decision",
		},
		{
			name: "forwarded with no decision",
			do: func(ctx context.Context) (*jsonrpc.Message, error) {
				note.Upstream(ctx, time.Millisecond)
				return okResult(), nil
			},
			decision: Allowed, status: StatusOK, reason: "forwarded with no policy decision recorded", latency: time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, _ := runLayer(t, params, tt.do)
			if e.Decision != tt.decision || e.Status != tt.status || e.ErrorCode != tt.code || e.Reason != tt.reason || e.Rule != tt.rule || e.Latency != tt.latency {
				t.Errorf("entry = decision %q status %q code %d reason %q rule %q latency %v\nwant      decision %q status %q code %d reason %q rule %q latency %v",
					e.Decision, e.Status, e.ErrorCode, e.Reason, e.Rule, e.Latency, tt.decision, tt.status, tt.code, tt.reason, tt.rule, tt.latency)
			}
			if e.Agent != "ci-bot" || e.Tool != "fs__write_file" || e.RequestID == "" || e.Time.IsZero() {
				t.Errorf("entry identity = agent %q tool %q id %q time %v", e.Agent, e.Tool, e.RequestID, e.Time)
			}
			if string(e.Arguments) != `{"password":"[REDACTED]","path":"/tmp/a"}` {
				t.Errorf("arguments = %s, want them redacted", e.Arguments)
			}
		})
	}
}

func TestLayerPassesTheReplyThrough(t *testing.T) {
	want := okResult()
	_, resp, err := runLayer(t, `{"name":"x__y"}`, func(context.Context) (*jsonrpc.Message, error) { return want, nil })
	if err != nil || resp != want {
		t.Errorf("reply = %v, %v; want the next layer's reply unchanged", resp, err)
	}
	wantErr := errors.New("boom")
	_, _, err = runLayer(t, `{"name":"x__y"}`, func(context.Context) (*jsonrpc.Message, error) { return nil, wantErr })
	if err != wantErr {
		t.Errorf("error = %v, want the next layer's error", err)
	}
}

func TestLayerCommitsEvenWhenALayerPanics(t *testing.T) {
	store := &fakeStore{}
	rec := NewRecorder(store, Options{})
	layer := NewLayer(&scriptedNext{do: func(context.Context) (*jsonrpc.Message, error) { panic("bug") }}, rec, nil)
	func() {
		defer func() { recover() }()
		layer.Call(asAgent("a"), "tools/call", json.RawMessage(`{"name":"x__y"}`))
	}()
	// If the slot had leaked, Close would wait until the timeout.
	closeRecorder(t, rec)
	if ids := store.ids(); len(ids) != 1 {
		t.Fatalf("%d entries, want 1", len(ids))
	}
	if e := store.entries[0]; e.Status != StatusNoResponse {
		t.Errorf("status = %q, want no_response", e.Status)
	}
}

func TestLayerRefusesWhenTheAuditLogIsFull(t *testing.T) {
	store := &fakeStore{hold: make(chan struct{})}
	defer close(store.hold)
	rec := NewRecorder(store, Options{QueueSize: 1, ReserveWait: testWait})
	next := &scriptedNext{do: func(context.Context) (*jsonrpc.Message, error) { return okResult(), nil }}
	layer := NewLayer(next, rec, nil)

	if resp, _ := layer.Call(asAgent("a"), "tools/call", json.RawMessage(`{"name":"x__y"}`)); resp.Error != nil {
		t.Fatalf("first call = %v, want it through", resp.Error)
	}
	resp, err := layer.Call(asAgent("a"), "tools/call", json.RawMessage(`{"name":"x__y"}`))
	if err != nil || resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError || resp.Error.Message != "audit log unavailable" {
		t.Fatalf("second call = %+v, %v; want -32603 audit log unavailable", resp, err)
	}
	if next.calls != 1 {
		t.Errorf("next layer called %d times, want 1: a refused call must not run", next.calls)
	}

	// A client that gives up while waiting gets no answer at all.
	ctx, cancel := context.WithCancel(asAgent("a"))
	cancel()
	if resp, err := layer.Call(ctx, "tools/call", json.RawMessage(`{"name":"x__y"}`)); resp != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call = %v, %v; want nil and context.Canceled", resp, err)
	}
}

func TestLayerIgnoresOtherMethods(t *testing.T) {
	store := &fakeStore{}
	rec := NewRecorder(store, Options{})
	next := &scriptedNext{do: func(context.Context) (*jsonrpc.Message, error) { return okResult(), nil }}
	layer := NewLayer(next, rec, nil)
	for _, m := range []string{"tools/list", "server/discover"} {
		if _, err := layer.Call(asAgent("a"), m, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	closeRecorder(t, rec)
	if n := len(store.ids()); n != 0 || next.calls != 2 {
		t.Errorf("entries = %d, next calls = %d; want 0 and 2", n, next.calls)
	}
}

func TestLayerBoundsWhatItStores(t *testing.T) {
	big := `{"name":"` + strings.Repeat("é", 300) + `","arguments":{"data":"` + strings.Repeat("x", maxStoredArguments) + `"}}`
	e, _, _ := runLayer(t, big, func(context.Context) (*jsonrpc.Message, error) { return okResult(), nil })
	if len(e.Tool) > maxStoredToolName || !utf8.ValidString(e.Tool) {
		t.Errorf("tool name: %d bytes, valid UTF-8 %v; want at most %d and valid", len(e.Tool), utf8.ValidString(e.Tool), maxStoredToolName)
	}
	if !strings.HasPrefix(string(e.Arguments), `{"[truncated]":true,"bytes":`) {
		t.Errorf("arguments = %.80s..., want the truncated marker", e.Arguments)
	}

	e, _, _ = runLayer(t, `not json`, func(context.Context) (*jsonrpc.Message, error) { return okResult(), nil })
	if e.Tool != "" || string(e.Arguments) != "null" {
		t.Errorf("unreadable params: tool %q args %s; want empty and null", e.Tool, e.Arguments)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	store := &fakeStore{}
	rec := NewRecorder(store, Options{})
	layer := NewLayer(&scriptedNext{do: func(context.Context) (*jsonrpc.Message, error) { return okResult(), nil }}, rec, nil)
	for range 100 {
		layer.Call(asAgent("a"), "tools/call", json.RawMessage(`{"name":"x__y"}`))
	}
	closeRecorder(t, rec)
	seen := make(map[string]bool)
	for _, id := range store.ids() {
		if seen[id] {
			t.Fatalf("request ID %s used twice", id)
		}
		seen[id] = true
	}
}

func TestUpstreamTimer(t *testing.T) {
	ctx, n := note.New(context.Background())
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timer := TimeUpstream(&scriptedNext{do: func(context.Context) (*jsonrpc.Message, error) {
		clock = clock.Add(42 * time.Millisecond)
		return okResult(), nil
	}})
	timer.now = func() time.Time { return clock }

	if _, err := timer.Call(ctx, "tools/list", nil); err != nil {
		t.Fatal(err)
	}
	if n.Get().Forwarded {
		t.Fatal("tools/list marked as a forwarded call")
	}
	if _, err := timer.Call(ctx, "tools/call", nil); err != nil {
		t.Fatal(err)
	}
	if got := n.Get(); !got.Forwarded || got.Latency != 42*time.Millisecond {
		t.Errorf("note = %+v, want forwarded with 42ms", got)
	}
}
