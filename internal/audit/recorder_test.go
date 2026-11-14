package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const testWait = 20 * time.Millisecond

// fakeStore records batches. It can be held (writes wait until released)
// or made to fail a number of times.
type fakeStore struct {
	mu       sync.Mutex
	entries  []Entry
	batches  []int
	failures int           // fail this many more writes
	hold     chan struct{} // if set, writes wait until it is closed
	attempts int
}

func (s *fakeStore) Write(ctx context.Context, entries []Entry) error {
	s.mu.Lock()
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.failures > 0 {
		s.failures--
		return errors.New("disk on fire")
	}
	s.entries = append(s.entries, entries...)
	s.batches = append(s.batches, len(entries))
	return nil
}

func (s *fakeStore) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, len(s.entries))
	for i, e := range s.entries {
		ids[i] = e.RequestID
	}
	return ids
}

// waitFor polls cond, since the writer runs on its own goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func record(t *testing.T, r *Recorder, id string) {
	t.Helper()
	slot, err := r.Reserve(context.Background())
	if err != nil {
		t.Fatalf("Reserve(%s) error = %v", id, err)
	}
	slot.Commit(Entry{RequestID: id})
}

func closeRecorder(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestRecorderWritesEverythingInOrder(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store, Options{BatchSize: 3})
	var want []string
	for i := range 10 {
		id := fmt.Sprintf("r%d", i)
		want = append(want, id)
		record(t, r, id)
	}
	closeRecorder(t, r)
	if got := strings.Join(store.ids(), ","); got != strings.Join(want, ",") {
		t.Fatalf("written = %s, want %s", got, strings.Join(want, ","))
	}
	for _, n := range store.batches {
		if n > 3 {
			t.Errorf("batch of %d, want at most 3", n)
		}
	}
}

func TestReserveFailsWhenQueueIsFull(t *testing.T) {
	store := &fakeStore{hold: make(chan struct{})}
	r := NewRecorder(store, Options{QueueSize: 2, ReserveWait: testWait})
	record(t, r, "a")
	record(t, r, "b")

	start := time.Now()
	if _, err := r.Reserve(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("third Reserve error = %v, want ErrUnavailable", err)
	}
	if waited := time.Since(start); waited < testWait {
		t.Errorf("gave up after %v, want at least the ReserveWait of %v", waited, testWait)
	}

	// Once the store catches up, slots free and calls go through again.
	close(store.hold)
	waitFor(t, "two entries written", func() bool { return len(store.ids()) == 2 })
	record(t, r, "c")
	closeRecorder(t, r)
	if got := strings.Join(store.ids(), ","); got != "a,b,c" {
		t.Errorf("written = %s, want a,b,c", got)
	}
}

func TestCallsInFlightHoldSlots(t *testing.T) {
	r := NewRecorder(&fakeStore{}, Options{QueueSize: 1, ReserveWait: testWait})
	slot, err := r.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The first call hasn't finished, so its slot is still taken.
	if _, err := r.Reserve(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second Reserve error = %v, want ErrUnavailable", err)
	}
	slot.Commit(Entry{RequestID: "a"})
	waitFor(t, "slot freed", func() bool {
		s, err := r.Reserve(context.Background())
		if err != nil {
			return false
		}
		s.Commit(Entry{RequestID: "b"})
		return true
	})
	closeRecorder(t, r)
}

func TestFailingStoreIsRetriedAndFillsTheQueue(t *testing.T) {
	store := &fakeStore{failures: 2}
	r := NewRecorder(store, Options{QueueSize: 1, ReserveWait: testWait})
	record(t, r, "a")

	// While the write is failing, the entry keeps its slot.
	if _, err := r.Reserve(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Reserve during failures error = %v, want ErrUnavailable", err)
	}
	waitFor(t, "entry written after retries", func() bool { return len(store.ids()) == 1 })
	closeRecorder(t, r)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.attempts != 3 {
		t.Errorf("attempts = %d, want 3 (two failures, then success)", store.attempts)
	}
}

func TestCloseWaitsForReservedCalls(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store, Options{})
	slot, err := r.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closed <- r.Close(ctx)
	}()

	// Close must wait for the call that is still running.
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v before the reserved call committed", err)
	case <-time.After(testWait):
	}
	// New calls are refused while closing.
	if _, err := r.Reserve(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Reserve while closing error = %v, want ErrUnavailable", err)
	}
	slot.Commit(Entry{RequestID: "late"})
	if err := <-closed; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := strings.Join(store.ids(), ","); got != "late" {
		t.Errorf("written = %q, want the late entry", got)
	}
}

func TestCloseGivesUpOnAStuckStore(t *testing.T) {
	store := &fakeStore{hold: make(chan struct{})}
	r := NewRecorder(store, Options{})
	record(t, r, "a")
	record(t, r, "b")

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	err := r.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "2 entries not written") {
		t.Fatalf("Close() error = %v, want one saying 2 entries were lost", err)
	}
	if _, err := r.Reserve(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Reserve after Close error = %v, want ErrUnavailable", err)
	}
}

func TestReserveStopsWhenTheCallIsCancelled(t *testing.T) {
	store := &fakeStore{hold: make(chan struct{})}
	defer close(store.hold)
	r := NewRecorder(store, Options{QueueSize: 1, ReserveWait: time.Hour})
	record(t, r, "a")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(testWait)
		cancel()
	}()
	if _, err := r.Reserve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reserve error = %v, want context.Canceled", err)
	}
}

func TestCommitTwiceWritesOnce(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store, Options{})
	slot, err := r.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slot.Commit(Entry{RequestID: "a"})
	slot.Commit(Entry{RequestID: "a-again"})
	closeRecorder(t, r)
	if got := strings.Join(store.ids(), ","); got != "a" {
		t.Errorf("written = %q, want just a", got)
	}
}

func TestConcurrentCallsAreAllRecorded(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store, Options{QueueSize: 8, ReserveWait: 5 * time.Second})
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			slot, err := r.Reserve(context.Background())
			if err != nil {
				t.Errorf("Reserve error = %v", err)
				return
			}
			slot.Commit(Entry{RequestID: fmt.Sprint(i)})
		})
	}
	wg.Wait()
	closeRecorder(t, r)
	if n := len(store.ids()); n != 200 {
		t.Fatalf("wrote %d entries, want 200", n)
	}
}
