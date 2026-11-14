// Package note lets the layers a tools/call passes through tell the audit
// log what they decided. The audit layer puts an empty Note in the request
// context; the policy guard, the rate limiter, and the upstream timer fill
// it in. Outside an audited call the functions do nothing.
//
// It is its own package, with no dependencies, so the policy and rate limit
// packages can use it without pulling in the audit store.
package note

import (
	"context"
	"sync"
	"time"
)

// Decision matches audit.Decision.
type Decision string

const (
	Allowed     Decision = "allowed"
	Denied      Decision = "denied"
	RateLimited Decision = "rate_limited"
)

// Note is what the layers found out about one call. Read it with Get after
// the call returns.
type Note struct {
	mu        sync.Mutex
	decision  Decision
	reason    string
	rule      string
	forwarded bool
	latency   time.Duration
}

// Snapshot is a copy of a Note's contents.
type Snapshot struct {
	Decision  Decision // empty if no layer decided
	Reason    string
	Rule      string
	Forwarded bool          // the call reached the upstream
	Latency   time.Duration // time spent upstream
}

type key struct{}

// New returns ctx with an empty Note, and the Note.
func New(ctx context.Context) (context.Context, *Note) {
	n := &Note{}
	return context.WithValue(ctx, key{}, n), n
}

func from(ctx context.Context) *Note {
	n, _ := ctx.Value(key{}).(*Note)
	return n
}

// Get copies the note's contents.
func (n *Note) Get() Snapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return Snapshot{Decision: n.decision, Reason: n.reason, Rule: n.rule, Forwarded: n.forwarded, Latency: n.latency}
}

func set(ctx context.Context, d Decision, reason, rule string) {
	n := from(ctx)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.decision, n.reason, n.rule = d, reason, rule
}

// Allow records that the policy allowed the call, and by which rule.
func Allow(ctx context.Context, reason, rule string) { set(ctx, Allowed, reason, rule) }

// Deny records that the policy refused the call. reason must never hold
// argument values.
func Deny(ctx context.Context, reason string) { set(ctx, Denied, reason, "") }

// Limit records that the rate limiter refused the call, and which limit
// entry applied.
func Limit(ctx context.Context, reason, entry string) { set(ctx, RateLimited, reason, entry) }

// Upstream records that the call was sent upstream and how long it took.
func Upstream(ctx context.Context, latency time.Duration) {
	n := from(ctx)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.forwarded, n.latency = true, latency
}
