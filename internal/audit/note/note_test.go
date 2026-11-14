package note

import (
	"context"
	"testing"
	"time"
)

func TestNoteRecordsTheLastDecision(t *testing.T) {
	ctx, n := New(context.Background())
	Allow(ctx, `role "r" allows x__y`, "r: x__y")
	Limit(ctx, "5/minute, burst 2", "x__y")
	Upstream(ctx, 3*time.Millisecond)

	want := Snapshot{Decision: RateLimited, Reason: "5/minute, burst 2", Rule: "x__y", Forwarded: true, Latency: 3 * time.Millisecond}
	if got := n.Get(); got != want {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
}

func TestDenyClearsTheRule(t *testing.T) {
	ctx, n := New(context.Background())
	Allow(ctx, "allowed", "r: x__y")
	Deny(ctx, "no")
	if got := n.Get(); got.Decision != Denied || got.Rule != "" || got.Reason != "no" {
		t.Errorf("Get() = %+v", got)
	}
}

func TestOutsideAnAuditedCallNothingHappens(t *testing.T) {
	ctx := context.Background()
	Allow(ctx, "a", "b")
	Deny(ctx, "a")
	Limit(ctx, "a", "b")
	Upstream(ctx, time.Second) // must not panic
}

func TestEmptyNote(t *testing.T) {
	_, n := New(context.Background())
	if got := n.Get(); got != (Snapshot{}) {
		t.Errorf("Get() = %+v, want empty", got)
	}
}

func TestEnsureSharesTheNoteAlreadyThere(t *testing.T) {
	outer, n := Ensure(context.Background())
	inner, m := Ensure(outer)
	if m != n {
		t.Fatal("Ensure made a second note when one was already in the context")
	}
	Deny(inner, "no")
	if got := n.Get(); got.Decision != Denied {
		t.Errorf("outer note = %+v, want the inner layer's decision", got)
	}
}
