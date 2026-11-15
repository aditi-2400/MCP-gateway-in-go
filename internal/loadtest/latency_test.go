package loadtest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// fakeCaller answers every call after an optional delay. reply picks the
// answer for call i (counting from 0, warm-up included).
type fakeCaller struct {
	calls int
	delay func(i int) time.Duration
	reply func(i int) (*jsonrpc.Message, error)
}

func (f *fakeCaller) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	i := f.calls
	f.calls++
	if f.delay != nil {
		time.Sleep(f.delay(i))
	}
	if f.reply != nil {
		return f.reply(i)
	}
	return okReply(), nil
}

func okReply() *jsonrpc.Message {
	return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","content":[{"type":"text","text":"3"}]}`)}
}

var testParams = json.RawMessage(`{"name":"add","arguments":{"a":1,"b":2}}`)

func TestSequentialTimesOnlyMeasuredCalls(t *testing.T) {
	// Warm-up calls are slow and measured ones fast, so a warm-up call that
	// was counted would stand out. Ten measured calls add up to more than
	// 50ms, so a clock that is not restarted for each call shows up too.
	c := &fakeCaller{delay: func(i int) time.Duration {
		if i < 3 {
			return 50 * time.Millisecond
		}
		return 10 * time.Millisecond
	}}
	got, err := Sequential(context.Background(), c, testParams, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 13 {
		t.Errorf("made %d calls, want 3 warm-up + 10 measured", c.calls)
	}
	if len(got) != 10 {
		t.Fatalf("got %d times, want 10", len(got))
	}
	for i, d := range got {
		if d < 10*time.Millisecond || d >= 50*time.Millisecond {
			t.Errorf("call %d took %v, want between 10ms and 50ms", i, d)
		}
	}
}

func TestSequentialStopsOnAFailedCall(t *testing.T) {
	tests := []struct {
		name  string
		reply func(i int) (*jsonrpc.Message, error)
		want  string
	}{
		{
			name: "no reply",
			reply: func(i int) (*jsonrpc.Message, error) {
				return nil, errors.New("connection closed")
			},
			want: "connection closed",
		},
		{
			name: "error reply",
			reply: func(i int) (*jsonrpc.Message, error) {
				return &jsonrpc.Message{Error: &jsonrpc.Error{Code: -32602, Message: "Unknown tool: add"}}, nil
			},
			want: "Unknown tool: add",
		},
		{
			name: "tool error",
			reply: func(i int) (*jsonrpc.Message, error) {
				return &jsonrpc.Message{Result: json.RawMessage(`{"isError":true,"content":[]}`)}, nil
			},
			want: "tool error",
		},
		{
			name: "unreadable result",
			reply: func(i int) (*jsonrpc.Message, error) {
				return &jsonrpc.Message{Result: json.RawMessage(`[1]`)}, nil
			},
			want: "result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fails on the 5th measured call, after 2 warm-up calls.
			c := &fakeCaller{reply: func(i int) (*jsonrpc.Message, error) {
				if i == 6 {
					return tt.reply(i)
				}
				return okReply(), nil
			}}
			got, err := Sequential(context.Background(), c, testParams, 2, 10)
			if err == nil {
				t.Fatalf("no error; got %d times", len(got))
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "call 5") {
				t.Errorf("error = %q, want it to name call 5 and contain %q", err, tt.want)
			}
			if c.calls != 7 {
				t.Errorf("made %d calls, want it to stop at the failed one (7)", c.calls)
			}
		})
	}
}

func TestSequentialFailingWarmUpIsAnError(t *testing.T) {
	c := &fakeCaller{reply: func(i int) (*jsonrpc.Message, error) {
		return nil, errors.New("upstream gone")
	}}
	if _, err := Sequential(context.Background(), c, testParams, 2, 10); err == nil || !strings.Contains(err.Error(), "warm-up") {
		t.Errorf("error = %v, want a warm-up error", err)
	}
}

func TestSequentialStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &fakeCaller{reply: func(i int) (*jsonrpc.Message, error) {
		if i == 3 {
			cancel()
		}
		return okReply(), nil
	}}
	_, err := Sequential(ctx, c, testParams, 0, 100)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if c.calls > 5 {
		t.Errorf("made %d calls after cancel", c.calls)
	}
}

func TestSequentialRejectsBadCounts(t *testing.T) {
	for _, tc := range []struct{ warmup, n int }{{0, 0}, {-1, 10}, {5, -1}} {
		if _, err := Sequential(context.Background(), &fakeCaller{}, testParams, tc.warmup, tc.n); err == nil {
			t.Errorf("warmup %d, n %d: no error", tc.warmup, tc.n)
		}
	}
}
