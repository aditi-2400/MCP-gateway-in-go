// Package upstream connects the gateway, acting as an MCP client, to
// upstream MCP servers.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// DefaultShutdownTimeout is how long Process.Close waits at each shutdown
// stage before escalating.
const DefaultShutdownTimeout = 5 * time.Second

// ErrClosed is returned for calls on a connection whose upstream has gone away.
var ErrClosed = errors.New("upstream: connection closed")

// Options configures a Conn or Process. Zero values pick defaults.
type Options struct {
	// MaxMessageSize limits each message read from the upstream.
	MaxMessageSize int
	// ShutdownTimeout is used by Process.Close.
	ShutdownTimeout time.Duration
	// OnNotification receives notifications sent by the upstream. It runs on
	// the read loop, so it must not block.
	OnNotification func(*jsonrpc.Message)
	// Logger records problems with upstream messages that are skipped.
	Logger *slog.Logger
	// Stderr receives the child process's stderr (Process only). Defaults to os.Stderr.
	Stderr io.Writer
}

func (o Options) withDefaults() Options {
	if o.MaxMessageSize <= 0 {
		o.MaxMessageSize = jsonrpc.DefaultMaxMessageSize
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// Conn is an MCP client connection to one upstream over a pair of streams.
// It is safe for concurrent use: many goroutines can Call at once, and each
// gets the response matching its own request.
type Conn struct {
	w              *jsonrpc.Writer
	onNotification func(*jsonrpc.Message)
	logger         *slog.Logger
	nextID         atomic.Int64

	mu      sync.Mutex
	pending map[jsonrpc.ID]chan *jsonrpc.Message
	err     error // set once, when the read loop stops

	done chan struct{} // closed when the read loop stops
}

// NewConn starts a read loop on r and returns a Conn that writes requests to w.
// The read loop runs until r returns EOF or an error.
func NewConn(r io.Reader, w io.Writer, opts Options) *Conn {
	opts = opts.withDefaults()
	c := &Conn{
		w:              jsonrpc.NewWriter(w),
		onNotification: opts.OnNotification,
		logger:         opts.Logger,
		pending:        make(map[jsonrpc.ID]chan *jsonrpc.Message),
		done:           make(chan struct{}),
	}
	go c.readLoop(jsonrpc.NewReader(r, opts.MaxMessageSize))
	return c
}

// Done is closed when the upstream stops sending (EOF or a read error).
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection stopped, or nil while it is running.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Call sends a request and waits for its response. The request gets a new ID
// chosen by the Conn, so callers never need to worry about ID collisions.
//
// An upstream error response is returned as a message with Error set and a
// nil error. A non-nil error means no response arrived: ctx was done or the
// upstream went away. Call never retries.
func (c *Conn) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("upstream: %s: %w", method, err)
	}

	id := jsonrpc.NewIntID(c.nextID.Add(1))
	// Buffered so the read loop can hand over the response without waiting,
	// even if this caller has already given up.
	ch := make(chan *jsonrpc.Message, 1)
	if err := c.addPending(id, ch); err != nil {
		return nil, err
	}

	if err := c.w.Write(jsonrpc.NewRequest(id, method, params)); err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("upstream: send %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		c.removePending(id)
		// Sent in the background: if the upstream has stopped reading its
		// input, the write could block, and the caller must not wait on that.
		go c.sendCancelled(id)
		return nil, fmt.Errorf("upstream: %s: %w", method, ctx.Err())
	case <-c.done:
		// The response may have arrived just before the connection closed.
		select {
		case resp := <-ch:
			return resp, nil
		default:
			return nil, c.Err()
		}
	}
}

// Notify sends a notification. Notifications have no response.
func (c *Conn) Notify(ctx context.Context, method string, params json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("upstream: %s: %w", method, err)
	}
	if err := c.Err(); err != nil {
		return err
	}
	if err := c.w.Write(jsonrpc.NewNotification(method, params)); err != nil {
		return fmt.Errorf("upstream: send %s: %w", method, err)
	}
	return nil
}

func (c *Conn) addPending(id jsonrpc.ID, ch chan *jsonrpc.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.pending[id] = ch
	return nil
}

func (c *Conn) removePending(id jsonrpc.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

// sendCancelled tells the upstream to stop work on a request, as the MCP
// stdio transport requires when a client abandons a request. The caller has
// already returned, so failures are logged.
func (c *Conn) sendCancelled(id jsonrpc.ID) {
	params, err := json.Marshal(struct {
		RequestID jsonrpc.ID `json:"requestId"`
		Reason    string     `json:"reason"`
	}{id, "request cancelled by gateway"})
	if err != nil {
		c.logger.Error("encode cancel notification", "id", id, "error", err)
		return
	}
	if err := c.w.Write(jsonrpc.NewNotification("notifications/cancelled", params)); err != nil {
		c.logger.Warn("send cancel notification", "id", id, "error", err)
	}
}

func (c *Conn) readLoop(r *jsonrpc.Reader) {
	defer close(c.done)
	for {
		msg, err := r.Read()
		if err != nil {
			if isBadMessage(err) {
				c.logger.Warn("skipping invalid message from upstream", "error", err)
				continue
			}
			c.stop(err)
			return
		}
		c.dispatch(msg)
	}
}

// isBadMessage reports whether err is about one bad line, after which the
// stream can still be read.
func isBadMessage(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) || errors.Is(err, jsonrpc.ErrMessageTooLarge)
}

func (c *Conn) stop(cause error) {
	err := ErrClosed
	if !errors.Is(cause, io.EOF) {
		err = fmt.Errorf("%w: %w", ErrClosed, cause)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *Conn) dispatch(msg *jsonrpc.Message) {
	switch msg.Kind() {
	case jsonrpc.KindResponse:
		if msg.ID == nil {
			c.logger.Warn("skipping upstream error response without an id", "error", msg.Error)
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if !ok {
			// Usually a late reply to a call that was already cancelled.
			c.logger.Debug("skipping upstream response with no waiting caller", "id", msg.ID)
			return
		}
		ch <- msg
	case jsonrpc.KindNotification:
		if c.onNotification != nil {
			c.onNotification(msg)
		}
	default:
		// Servers must not send requests in protocol version 2026-07-28.
		c.logger.Warn("skipping request sent by upstream", "method", msg.Method)
	}
}
