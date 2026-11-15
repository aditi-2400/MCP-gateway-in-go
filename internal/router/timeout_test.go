package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// hangingUpstream never answers. It waits until its context ends, and
// remembers the deadline each call was given. If the context never ends it
// gives up after hangLimit with errHungTooLong, so a broken timeout fails
// the test quickly instead of hanging it.
// hangLimit is longer than any timeout these tests expect to fire.
const hangLimit = 2 * time.Second

var errHungTooLong = errors.New("upstream call was never ended by a timeout")

type hangingUpstream struct {
	mu        sync.Mutex
	deadlines []time.Time // zero when a call had no deadline
}

func (h *hangingUpstream) Call(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
	deadline, _ := ctx.Deadline()
	h.mu.Lock()
	h.deadlines = append(h.deadlines, deadline)
	h.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(hangLimit):
		return nil, errHungTooLong
	}
}

func (h *hangingUpstream) lastDeadline(t *testing.T) time.Time {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.deadlines) == 0 {
		t.Fatal("upstream was never called")
	}
	return h.deadlines[len(h.deadlines)-1]
}

func TestToolsCallStopsAtTheUpstreamTimeout(t *testing.T) {
	slow := &hangingUpstream{}
	r := newRouter(t, Named{Name: "slow", Upstream: slow, Timeout: 20 * time.Millisecond})

	// The client's context has no deadline, so only the upstream's own
	// timeout can end the call.
	start := time.Now()
	resp, err := r.Call(context.Background(), "tools/call", callParams(`"name":"slow__x"`))
	if !errors.Is(err, context.DeadlineExceeded) || resp != nil {
		t.Fatalf("Call() = %v, %v; want nil and DeadlineExceeded", resp, err)
	}
	if took := time.Since(start); took > testTimeout {
		t.Fatalf("call took %v", took)
	}
	if !strings.Contains(err.Error(), "slow") {
		t.Errorf("error %q does not name the upstream", err)
	}
}

func TestUpstreamTimeoutIsPerUpstream(t *testing.T) {
	short, none := &hangingUpstream{}, &hangingUpstream{}
	r := newRouter(t,
		Named{Name: "short", Upstream: short, Timeout: 3 * time.Second},
		Named{Name: "none", Upstream: none},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Call(ctx, "tools/call", callParams(`"name":"none__x"`))
	}()
	before := time.Now()
	go r.Call(ctx, "tools/call", callParams(`"name":"short__x"`))

	waitFor(t, func() bool { short.mu.Lock(); defer short.mu.Unlock(); return len(short.deadlines) == 1 })
	waitFor(t, func() bool { none.mu.Lock(); defer none.mu.Unlock(); return len(none.deadlines) == 1 })
	if d := short.lastDeadline(t); d.Before(before.Add(3*time.Second)) || d.After(time.Now().Add(3*time.Second)) {
		t.Errorf("short got deadline %v, want about 3s after the call", d)
	}
	if d := none.lastDeadline(t); !d.IsZero() {
		t.Errorf("upstream without a timeout got deadline %v", d)
	}
	cancel()
	<-done
}

// A shorter deadline from the client (the gateway's -call-timeout) still
// wins over a longer upstream timeout.
func TestTheShorterTimeoutWins(t *testing.T) {
	slow := &hangingUpstream{}
	r := newRouter(t, Named{Name: "slow", Upstream: slow, Timeout: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	clientDeadline, _ := ctx.Deadline()
	if _, err := r.Call(ctx, "tools/call", callParams(`"name":"slow__x"`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := slow.lastDeadline(t); !d.Equal(clientDeadline) {
		t.Errorf("upstream got deadline %v, want the client's %v", d, clientDeadline)
	}
}

// tools/list uses the same timeout: a slow upstream is left out and the
// others are still listed.
func TestToolsListLeavesOutAnUpstreamThatTimesOut(t *testing.T) {
	r := newRouter(t,
		Named{Name: "slow", Upstream: &hangingUpstream{}, Timeout: 20 * time.Millisecond},
		Named{Name: "good", Upstream: listing(`{"name":"echo"}`)},
	)
	if got := strings.Join(toolNames(t, r), " "); got != "good__echo" {
		t.Fatalf("tools = %s, want good__echo", got)
	}
}

func TestNewRejectsANegativeTimeout(t *testing.T) {
	if _, err := New([]Named{{Name: "fs", Upstream: listing(), Timeout: -time.Second}}, Options{}); err == nil {
		t.Fatal("New() error = nil, want an error")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(testTimeout); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in time")
}
