package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Decision says what the gateway did with a call.
type Decision string

const (
	Allowed     Decision = "allowed"
	Denied      Decision = "denied"       // by the policy
	RateLimited Decision = "rate_limited" // by the rate limiter
)

// Status says how a call ended.
type Status string

const (
	StatusOK         Status = "ok"          // the tool returned a result
	StatusToolError  Status = "tool_error"  // a result with isError: true
	StatusRPCError   Status = "rpc_error"   // a JSON-RPC error; see ErrorCode
	StatusNoResponse Status = "no_response" // timed out, cancelled, or upstream gone
)

// Entry is one audited tools/call.
type Entry struct {
	Time      time.Time
	RequestID string // the gateway's own ID for the call, unique per entry
	Agent     string
	Tool      string
	Arguments json.RawMessage // already redacted
	Decision  Decision
	Reason    string // why, in words; never contains argument values
	Rule      string // the policy rule or limit entry involved
	Status    Status
	ErrorCode int           // set when Status is StatusRPCError
	Latency   time.Duration // time spent upstream; 0 if not forwarded
}

// Store saves entries. Write gets entries in the order calls committed
// them. It must save all or none, so a retry can't leave half a batch, and
// must not keep the slice after returning (the writer reuses it).
type Store interface {
	Write(ctx context.Context, entries []Entry) error
}

// ErrUnavailable means no queue slot freed up in time, or the recorder is
// closed. The call must be refused: nothing runs unrecorded.
var ErrUnavailable = errors.New("audit log unavailable")

// Defaults for Options.
const (
	DefaultQueueSize   = 1024
	DefaultReserveWait = 100 * time.Millisecond
	DefaultBatchSize   = 100
	retryDelayStart    = 100 * time.Millisecond
	retryDelayMax      = 5 * time.Second
)

// Options configures a Recorder. Zero values pick the defaults.
type Options struct {
	// QueueSize caps calls in flight plus entries not yet written.
	QueueSize int
	// ReserveWait is how long a call waits for a free slot before it is
	// refused.
	ReserveWait time.Duration
	// BatchSize caps entries per Store.Write.
	BatchSize int
	Logger    *slog.Logger
}

// Recorder queues entries and writes them to a Store in the background.
//
// Every call reserves a slot before it is forwarded, and the slot is only
// freed once its entry is written. So the number of slots bounds both the
// calls in flight and the entries waiting to be written, and when the store
// falls behind or fails, new calls are refused instead of running without
// a record.
type Recorder struct {
	store       Store
	reserveWait time.Duration
	batchSize   int
	logger      *slog.Logger

	slots chan struct{} // one value per reserved slot
	queue chan Entry    // committed entries; never fuller than slots

	mu      sync.Mutex
	closed  bool
	pending sync.WaitGroup // reserved slots whose entry isn't written yet

	ctx    context.Context // cancelled when Close gives up
	cancel context.CancelFunc
	done   chan struct{} // closed when the writer exits
}

// NewRecorder starts a Recorder writing to store.
func NewRecorder(store Store, opts Options) *Recorder {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.ReserveWait <= 0 {
		opts.ReserveWait = DefaultReserveWait
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Recorder{
		store:       store,
		reserveWait: opts.ReserveWait,
		batchSize:   opts.BatchSize,
		logger:      opts.Logger,
		slots:       make(chan struct{}, opts.QueueSize),
		queue:       make(chan Entry, opts.QueueSize),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	go r.write()
	return r
}

// Slot is a reserved place for one call's entry. Commit it exactly once.
type Slot struct {
	r    *Recorder
	once sync.Once
}

// Reserve holds a slot for one call. It waits up to ReserveWait for one to
// free up, then returns ErrUnavailable. If ctx ends first it returns
// ctx.Err().
func (r *Recorder) Reserve(ctx context.Context) (*Slot, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrUnavailable
	}
	// Counted under the lock, so Close never starts waiting while a
	// Reserve is still about to add to the count.
	r.pending.Add(1)
	r.mu.Unlock()

	select {
	case r.slots <- struct{}{}:
		return &Slot{r: r}, nil
	default:
	}
	timer := time.NewTimer(r.reserveWait)
	defer timer.Stop()
	select {
	case r.slots <- struct{}{}:
		return &Slot{r: r}, nil
	case <-timer.C:
		r.pending.Done()
		return nil, ErrUnavailable
	case <-ctx.Done():
		r.pending.Done()
		return nil, ctx.Err()
	}
}

// Commit queues the entry. It never blocks: the queue has as much room as
// there are slots, and this slot is one of them. Calls after the first do
// nothing.
func (s *Slot) Commit(e Entry) {
	s.once.Do(func() {
		s.r.queue <- e
	})
}

// write runs in its own goroutine until Close.
func (r *Recorder) write() {
	defer close(r.done)
	batch := make([]Entry, 0, r.batchSize)
	for {
		select {
		case e := <-r.queue:
			batch = append(batch[:0], e)
		case <-r.ctx.Done():
			return
		}
		// Take whatever else is already waiting, up to the batch size.
	fill:
		for len(batch) < r.batchSize {
			select {
			case e := <-r.queue:
				batch = append(batch, e)
			default:
				break fill
			}
		}
		if !r.writeWithRetry(batch) {
			return
		}
		for range batch {
			<-r.slots
			r.pending.Done()
		}
	}
}

// writeWithRetry keeps trying until the batch is saved or Close gives up.
// Retrying is safe here: it repeats the gateway's own log write, never a
// tool call. While it retries, slots stay taken, so new calls are refused.
func (r *Recorder) writeWithRetry(batch []Entry) bool {
	delay := retryDelayStart
	for {
		err := r.store.Write(r.ctx, batch)
		if err == nil {
			return true
		}
		r.logger.Error("audit write failed; new calls will be refused once the queue is full", "entries", len(batch), "error", err, "retry_in", delay)
		select {
		case <-time.After(delay):
		case <-r.ctx.Done():
			r.logger.Error("audit entries lost at shutdown", "entries", len(batch))
			return false
		}
		delay = min(delay*2, retryDelayMax)
	}
}

// Close stops new reservations, waits for reserved calls to commit and for
// every entry to be written, and stops the writer. If ctx ends first, the
// entries not yet written are lost and Close says how many. In that case
// the goroutine waiting for the flush stays blocked; Close only runs at
// shutdown, so the process ends soon after.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	flushed := make(chan struct{})
	go func() {
		r.pending.Wait()
		close(flushed)
	}()
	var err error
	select {
	case <-flushed:
	case <-ctx.Done():
		err = fmt.Errorf("audit: %d entries not written before shutdown: %w", len(r.slots), ctx.Err())
	}
	r.cancel()
	<-r.done
	return err
}
