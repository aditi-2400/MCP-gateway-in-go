package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		count   int
		per     time.Duration
		wantErr bool
	}{
		{in: "5/minute", count: 5, per: time.Minute},
		{in: "100/second", count: 100, per: time.Second},
		{in: "1/hour", count: 1, per: time.Hour},
		{in: "1000000/second", count: 1_000_000, per: time.Second},
		{in: "", wantErr: true},
		{in: "5", wantErr: true},
		{in: "0/minute", wantErr: true},
		{in: "-1/minute", wantErr: true},
		{in: "1000001/second", wantErr: true},
		{in: "99999999999999999999/second", wantErr: true},
		{in: "1.5/second", wantErr: true},
		{in: "5/min", wantErr: true},
		{in: "5/minutes", wantErr: true},
		{in: "5/Minute", wantErr: true},
		{in: " 5/minute", wantErr: true},
		{in: "5 /minute", wantErr: true},
		{in: "5/minute/x", wantErr: true},
		{in: "/minute", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			count, per, err := ParseRate(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRate(%q) error = %v, want error %v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && (count != tt.count || per != tt.per) {
				t.Errorf("ParseRate(%q) = %d per %v, want %d per %v", tt.in, count, per, tt.count, tt.per)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		lim  Limit
		ok   bool
	}{
		{name: "valid", lim: Limit{Count: 5, Per: time.Minute, Burst: 2}, ok: true},
		{name: "zero count", lim: Limit{Count: 0, Per: time.Minute, Burst: 1}},
		{name: "zero burst", lim: Limit{Count: 5, Per: time.Minute, Burst: 0}},
		{name: "huge burst", lim: Limit{Count: 5, Per: time.Minute, Burst: maxCount + 1}},
		{name: "odd period", lim: Limit{Count: 5, Per: 7 * time.Second, Burst: 1}},
		{name: "zero value", lim: Limit{}},
	}
	for _, tt := range tests {
		if err := tt.lim.Validate(); (err == nil) != tt.ok {
			t.Errorf("%s: Validate() = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

// step is one call at a moment, with what it should get.
type step struct {
	after time.Duration // clock moves this much before the call
	allow bool
	retry time.Duration // when denied
}

func runSteps(t *testing.T, lim Limit, steps []step) {
	t.Helper()
	clock := newFakeClock()
	l := New(clock.now)
	for i, s := range steps {
		clock.advance(s.after)
		allowed, retry := l.Allow("bot", "fs__read", lim)
		if allowed != s.allow || retry != s.retry {
			t.Fatalf("call %d (+%v): Allow() = %v, %v; want %v, %v", i, s.after, allowed, retry, s.allow, s.retry)
		}
	}
}

func TestBurstThenRefill(t *testing.T) {
	runSteps(t, Limit{Count: 1, Per: time.Second, Burst: 3}, []step{
		{allow: true},
		{allow: true},
		{allow: true},
		{retry: time.Second}, // burst used up
		{after: 400 * time.Millisecond, retry: 600 * time.Millisecond}, // part way to a token
		{after: 600 * time.Millisecond, allow: true},                   // one token back
		{retry: time.Second},
		{after: time.Hour, allow: true}, // a long pause refills only up to Burst
		{allow: true},
		{allow: true},
		{retry: time.Second},
	})
}

func TestSlowRate(t *testing.T) {
	// 5/minute is one token every 12 seconds.
	runSteps(t, Limit{Count: 5, Per: time.Minute, Burst: 1}, []step{
		{allow: true},
		{retry: 12 * time.Second},
		{after: 12*time.Second - time.Nanosecond, retry: time.Nanosecond},
		{after: time.Nanosecond, allow: true},
	})
}

func TestSteadyRateNeverExceeded(t *testing.T) {
	// Calling every millisecond from 0s through 60s, both ends included, gets
	// exactly Burst + Count: refills land at 1s, 2s, ..., 60s. (Stopping at
	// 59.999s would get 69, since the 60th refill lands at 60s.)
	clock := newFakeClock()
	l := New(clock.now)
	lim := Limit{Count: 60, Per: time.Minute, Burst: 10}
	allowed := 0
	for range 60_001 {
		if ok, _ := l.Allow("bot", "x__y", lim); ok {
			allowed++
		}
		clock.advance(time.Millisecond)
	}
	if allowed != 70 {
		t.Fatalf("allowed %d calls in a minute, want 70 (burst 10 + 60 refilled)", allowed)
	}
}

func TestRoundsIntervalUp(t *testing.T) {
	// 3/second can't be split into whole nanoseconds. Rounding up means
	// three tokens take at least a full second, never less.
	lim := Limit{Count: 3, Per: time.Second, Burst: 1}
	if got := lim.interval(); got != 333_333_334 {
		t.Fatalf("interval = %v, want 333.333334ms", got)
	}
}

func TestBucketsAreSeparate(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)
	lim := Limit{Count: 1, Per: time.Minute, Burst: 1}
	calls := []struct{ agent, tool string }{
		{"bot", "fs__read"},
		{"bot", "fs__write"},
		{"other", "fs__read"},
	}
	for _, c := range calls {
		if ok, _ := l.Allow(c.agent, c.tool, lim); !ok {
			t.Fatalf("first call for %s/%s denied", c.agent, c.tool)
		}
	}
	for _, c := range calls {
		if ok, _ := l.Allow(c.agent, c.tool, lim); ok {
			t.Fatalf("second call for %s/%s allowed, want its own empty bucket", c.agent, c.tool)
		}
	}
}

func TestInvalidLimitDeniesAll(t *testing.T) {
	l := New(newFakeClock().now)
	for _, lim := range []Limit{{}, {Count: 5, Per: time.Minute}, {Count: 5, Burst: 5}} {
		if ok, _ := l.Allow("bot", "x__y", lim); ok {
			t.Errorf("Allow with %+v = true, want false", lim)
		}
	}
}

func TestClockGoingBackGivesNoExtraTokens(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)
	lim := Limit{Count: 1, Per: time.Second, Burst: 1}
	if ok, _ := l.Allow("bot", "x__y", lim); !ok {
		t.Fatal("first call denied")
	}
	clock.advance(-time.Hour)
	if ok, retry := l.Allow("bot", "x__y", lim); ok || retry <= 0 {
		t.Fatalf("after the clock went back: Allow() = %v, %v; want denied", ok, retry)
	}
}

func TestConcurrentCallsShareOneBucket(t *testing.T) {
	l := New(newFakeClock().now) // frozen clock: no refills
	lim := Limit{Count: 1, Per: time.Hour, Burst: 50}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if ok, _ := l.Allow("bot", "x__y", lim); ok {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 50 {
		t.Fatalf("allowed %d of 200 concurrent calls, want exactly the burst of 50", got)
	}
}

func TestSweepDropsOnlyFullBuckets(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)
	fast := Limit{Count: 1, Per: time.Second, Burst: 1}
	slow := Limit{Count: 1, Per: time.Hour, Burst: 1}
	for i := range 100 {
		l.Allow("bot", "fs__made-up-"+string(rune('a'+i%26))+string(rune('a'+i/26)), fast)
	}
	l.Allow("bot", "email__send", slow)
	if got := l.size(); got != 101 {
		t.Fatalf("size = %d, want 101", got)
	}

	// After a minute the fast buckets are full again; the slow one is not.
	clock.advance(sweepInterval)
	l.Allow("bot", "fs__read", fast) // any call triggers the sweep
	if got := l.size(); got != 2 {
		t.Fatalf("size after sweep = %d, want 2 (email__send and fs__read)", got)
	}
	// The slow bucket kept its state.
	if ok, _ := l.Allow("bot", "email__send", slow); ok {
		t.Error("email__send allowed again after the sweep; its bucket was dropped while still empty")
	}
}

func TestString(t *testing.T) {
	if got := (Limit{Count: 5, Per: time.Minute, Burst: 2}).String(); got != "5/minute, burst 2" {
		t.Errorf("String() = %q", got)
	}
}
