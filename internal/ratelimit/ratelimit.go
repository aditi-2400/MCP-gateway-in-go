// Package ratelimit limits how often each agent may call each tool, with one
// token bucket per (agent, tool) pair.
//
// A bucket holds up to Burst tokens and gains Count tokens every Per. Each
// call takes one token; a call that finds the bucket empty is refused, along
// with how long until the next token arrives.
//
// Each bucket is stored as a single time: the moment it will be full again.
// This is the usual compact way to write a token bucket (also known as GCRA,
// the generic cell rate algorithm). With T the time one token takes to come
// back, the bucket holds Burst - (full - now)/T tokens. Taking a token pushes
// "full" one T later, and a call is allowed as long as that keeps "full" at
// most Burst*T away. All the math stays in whole nanoseconds, so there is no
// rounding drift.
package ratelimit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxCount caps Count and Burst. It keeps Burst*T inside time.Duration even
// at the slowest rate (1/hour), and no real limit needs more.
const maxCount = 1_000_000

// sweepInterval is how often idle buckets are dropped.
const sweepInterval = time.Minute

// Limit is one rate: Count calls per Per, with up to Burst at once.
type Limit struct {
	Count int
	Per   time.Duration
	Burst int
}

var units = map[string]time.Duration{
	"second": time.Second,
	"minute": time.Minute,
	"hour":   time.Hour,
}

// ParseRate reads a rate like "5/minute". The unit is second, minute, or
// hour, and the count a whole number.
func ParseRate(s string) (count int, per time.Duration, err error) {
	countText, unit, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, fmt.Errorf("rate %q must look like 5/minute", s)
	}
	per, ok := units[unit]
	if !ok {
		return 0, 0, fmt.Errorf("rate %q: unit must be second, minute, or hour", s)
	}
	count, err = strconv.Atoi(countText)
	if err != nil || count < 1 || count > maxCount {
		return 0, 0, fmt.Errorf("rate %q: count must be a whole number from 1 to %d", s, maxCount)
	}
	return count, per, nil
}

// Validate checks that the limit can be used.
func (l Limit) Validate() error {
	var errs []error
	if l.Count < 1 || l.Count > maxCount {
		errs = append(errs, fmt.Errorf("count must be from 1 to %d", maxCount))
	}
	if l.Per != time.Second && l.Per != time.Minute && l.Per != time.Hour {
		errs = append(errs, errors.New("period must be a second, minute, or hour"))
	}
	if l.Burst < 1 || l.Burst > maxCount {
		errs = append(errs, fmt.Errorf("burst must be from 1 to %d", maxCount))
	}
	return errors.Join(errs...)
}

func (l Limit) String() string {
	for name, d := range units {
		if d == l.Per {
			return fmt.Sprintf("%d/%s, burst %d", l.Count, name, l.Burst)
		}
	}
	return fmt.Sprintf("%d per %v, burst %d", l.Count, l.Per, l.Burst)
}

// interval is how long one token takes to come back. It rounds up, so a
// limit is never looser than written.
func (l Limit) interval() time.Duration {
	count := time.Duration(l.Count)
	return (l.Per + count - 1) / count
}

type key struct{ agent, tool string }

// Limiter holds the buckets. It is safe for concurrent use.
type Limiter struct {
	now func() time.Time

	mu        sync.Mutex
	full      map[key]time.Time // when each bucket is full again
	lastSweep time.Time
}

// New returns a Limiter that reads the time from now (time.Now in
// production; tests pass a fake clock).
func New(now func() time.Time) *Limiter {
	t := now()
	return &Limiter{now: now, full: make(map[key]time.Time), lastSweep: t}
}

// Allow takes one token from the bucket for agent and tool, using lim. If
// the bucket is empty it returns false and how long until a token is back.
// An invalid limit refuses every call, since a broken limit must not mean
// "no limit".
func (l *Limiter) Allow(agent, tool string, lim Limit) (allowed bool, retryAfter time.Duration) {
	if lim.Validate() != nil {
		return false, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	k := key{agent, tool}
	full := now
	if stored, ok := l.full[k]; ok && stored.After(now) {
		full = stored
	}
	t := lim.interval()
	next := full.Add(t)
	// The call fits if, after taking a token, the bucket is still at most
	// Burst tokens from full.
	earliest := next.Add(-t * time.Duration(lim.Burst))
	if now.Before(earliest) {
		return false, earliest.Sub(now)
	}
	l.full[k] = next
	return true, 0
}

// sweep drops buckets that have refilled. A full bucket acts exactly like a
// new one, so dropping it changes nothing, and memory stays bounded even if
// a caller invents a new tool name on every call.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for k, full := range l.full {
		if !full.After(now) {
			delete(l.full, k)
		}
	}
}

// size reports how many buckets are stored, for tests.
func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.full)
}
