// Package proxy forwards MCP messages between a client and an upstream server.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/progress"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

// DefaultCallTimeout is how long a forwarded request may wait for the upstream.
const DefaultCallTimeout = 60 * time.Second

// Handler answers the client requests and notifications the proxy reads.
// The gateway uses a router.Router; upstream.Conn also fits, and tests use
// fakes.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
	Notify(ctx context.Context, method string, params json.RawMessage) error
}

// Options configures a Proxy. Zero values pick defaults.
type Options struct {
	// CallTimeout limits each forwarded request.
	CallTimeout time.Duration
	// MaxMessageSize limits each message read from the client.
	MaxMessageSize int
	Logger         *slog.Logger
	// Progress routes upstream progress notifications back to the request
	// that asked for them. If nil, progress tokens are passed through as is.
	Progress *progress.Tracker
}

// Proxy runs one client's session: it reads requests, passes each to the
// Handler with a timeout, and writes the replies back with the client's IDs.
type Proxy struct {
	handler        Handler
	callTimeout    time.Duration
	maxMessageSize int
	logger         *slog.Logger
	progress       *progress.Tracker

	mu     sync.Mutex
	client *jsonrpc.Writer // set while Serve runs, for upstream notifications
}

// New returns a Proxy that passes client messages to h.
func New(h Handler, opts Options) *Proxy {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.MaxMessageSize <= 0 {
		opts.MaxMessageSize = jsonrpc.DefaultMaxMessageSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Proxy{
		handler:        h,
		callTimeout:    opts.CallTimeout,
		maxMessageSize: opts.MaxMessageSize,
		logger:         opts.Logger,
		progress:       opts.Progress,
	}
}

// Serve reads client messages from in and writes replies to out.
//
// When in reaches EOF, Serve stops reading but lets in-flight requests finish
// (each still bounded by CallTimeout), because the client can still read
// replies. It then returns nil. If ctx is done or a read or write fails, Serve
// cancels in-flight requests, waits for them, and returns the error.
//
// Reading in cannot be interrupted, so if Serve returns because ctx is done,
// one goroutine stays blocked on in until it is closed.
func (p *Proxy) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	s := &session{
		p:        p,
		ctx:      ctx,
		fail:     cancel,
		w:        jsonrpc.NewWriter(out),
		inflight: make(map[jsonrpc.ID]context.CancelFunc),
	}
	p.setClient(s.w)
	defer p.setClient(nil)

	err := s.run(jsonrpc.NewReader(in, p.maxMessageSize))
	if err == nil {
		// Client EOF. It may still be reading, so let in-flight requests reply.
		s.drain()
		err = context.Cause(ctx)
	}
	// Nobody will read replies to in-flight requests now, so stop them.
	cancel(err)
	s.wg.Wait()
	return err
}

// ForwardNotification sends an upstream notification to the connected
// client unchanged. Pass it as upstream.Options.OnNotification.
func (p *Proxy) ForwardNotification(msg *jsonrpc.Message) {
	p.mu.Lock()
	w := p.client
	p.mu.Unlock()
	if w == nil {
		p.logger.Debug("dropping upstream notification: no client connected", "method", msg.Method)
		return
	}
	if err := w.Write(msg); err != nil {
		p.logger.Warn("forward upstream notification", "method", msg.Method, "error", err)
	}
}

func (p *Proxy) setClient(w *jsonrpc.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = w
}

// Forward passes one client request to h with a timeout and builds the reply
// for the client, with the client's own request ID. It returns nil when no
// reply should be sent, because ctx was cancelled (the client cancelled the
// request or went away). Failures with no answer become -32603 errors whose
// details go to logger only. Forward never retries.
//
// Both the stdio session and the HTTP frontend use it, so the two transports
// answer the same way.
func Forward(ctx context.Context, h Handler, timeout time.Duration, req *jsonrpc.Message, logger *slog.Logger) *jsonrpc.Message {
	id := *req.ID
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := h.Call(callCtx, req.Method, req.Params)
	if err == nil && resp != nil {
		// The upstream saw the gateway's own request ID; the client gets its own back.
		if resp.Error != nil {
			return jsonrpc.NewErrorResponse(&id, resp.Error)
		}
		return jsonrpc.NewResult(id, resp.Result)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		err = errors.New("upstream returned no response and no error")
	}

	// Details go to the log only, so internal paths and hosts do not leak to clients.
	logger.Warn("upstream call failed", "method", req.Method, "error", err)
	msg := "upstream error"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg = "upstream timed out"
	case errors.Is(err, upstream.ErrClosed):
		msg = "upstream unavailable"
	}
	return jsonrpc.NewErrorResponse(&id, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: msg})
}

// ForwardWithProgress is Forward plus progress routing. If req has a
// _meta.progressToken and tracker is set, the upstream gets a gateway token,
// and progress for it goes to notify until the reply is ready. reply is then
// called exactly once, with Forward's result (nil means send nothing), and no
// progress can follow it.
func ForwardWithProgress(ctx context.Context, h Handler, timeout time.Duration, req *jsonrpc.Message, logger *slog.Logger,
	tracker *progress.Tracker, notify, reply func(*jsonrpc.Message)) {
	if tracker == nil {
		reply(Forward(ctx, h, timeout, req, logger))
		return
	}
	stream := progress.NewStream(notify)
	params, release, _ := tracker.Track(req.Params, stream.Send)
	forwarded := *req
	forwarded.Params = params

	resp := Forward(ctx, h, timeout, &forwarded, logger)
	release()
	stream.Finish(func() { reply(resp) })
}

// session is the state of one Serve call.
type session struct {
	p    *Proxy
	ctx  context.Context
	fail context.CancelCauseFunc
	w    *jsonrpc.Writer
	wg   sync.WaitGroup

	mu       sync.Mutex
	inflight map[jsonrpc.ID]context.CancelFunc // client request ID -> cancel
}

type readResult struct {
	msg *jsonrpc.Message
	err error
}

func (s *session) run(r *jsonrpc.Reader) error {
	msgs := readMessages(s.ctx, r)
	for {
		select {
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		case rr, ok := <-msgs:
			if !ok {
				return context.Cause(s.ctx)
			}
			switch {
			case rr.err == nil:
				s.handle(rr.msg)
			case isBadMessage(rr.err):
				s.write(badMessageResponse(rr.err))
			case errors.Is(rr.err, io.EOF):
				return nil
			default:
				return fmt.Errorf("proxy: read from client: %w", rr.err)
			}
		}
	}
}

// readMessages reads client messages on its own goroutine so that run can
// also watch ctx. It stops after the first stream error.
func readMessages(ctx context.Context, r *jsonrpc.Reader) <-chan readResult {
	ch := make(chan readResult)
	go func() {
		defer close(ch)
		for {
			msg, err := r.Read()
			select {
			case ch <- readResult{msg, err}:
			case <-ctx.Done():
				return
			}
			if err != nil && !isBadMessage(err) {
				return
			}
		}
	}()
	return ch
}

func (s *session) handle(msg *jsonrpc.Message) {
	switch msg.Kind() {
	case jsonrpc.KindRequest:
		s.startRequest(msg)
	case jsonrpc.KindNotification:
		if msg.Method == "notifications/cancelled" {
			// The client's cancel names the client's request ID, which the
			// upstream never saw. Cancelling the call makes the upstream
			// Conn send its own cancel with the right ID.
			s.cancelRequest(msg.Params)
			return
		}
		if err := s.p.handler.Notify(s.ctx, msg.Method, msg.Params); err != nil {
			s.p.logger.Warn("forward client notification", "method", msg.Method, "error", err)
		}
	default:
		s.p.logger.Warn("ignoring response sent by client", "id", msg.ID)
	}
}

// startRequest forwards a request on its own goroutine so a slow tool does
// not hold up the client's other requests.
func (s *session) startRequest(req *jsonrpc.Message) {
	id := *req.ID
	ctx, cancel := context.WithCancel(s.ctx)

	s.mu.Lock()
	_, duplicate := s.inflight[id]
	if !duplicate {
		s.inflight[id] = cancel
	}
	s.mu.Unlock()

	if duplicate {
		cancel()
		s.write(jsonrpc.NewErrorResponse(&id, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidRequest,
			Message: "request id is already in use by an in-flight request",
		}))
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer s.untrack(id)
		ForwardWithProgress(ctx, s.p.handler, s.p.callTimeout, req, s.p.logger, s.p.progress, s.write, func(resp *jsonrpc.Message) {
			if resp != nil {
				s.write(resp)
			}
		})
	}()
}

// drain waits for in-flight requests to finish, or for the session to be
// cancelled. Only call it after run returns, so no new requests start.
func (s *session) drain() {
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-s.ctx.Done():
	}
}

func (s *session) untrack(id jsonrpc.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

func (s *session) cancelRequest(params json.RawMessage) {
	var p struct {
		RequestID *jsonrpc.ID `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.RequestID == nil {
		s.p.logger.Warn("ignoring malformed cancel notification", "params", string(params))
		return
	}
	s.mu.Lock()
	cancel, ok := s.inflight[*p.RequestID]
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

// write sends a message to the client. A failed write means the client is
// gone, so it ends the session.
func (s *session) write(msg *jsonrpc.Message) {
	if err := s.w.Write(msg); err != nil {
		s.fail(fmt.Errorf("proxy: write to client: %w", err))
	}
}

// isBadMessage reports whether err is about one bad line, after which the
// stream can still be read.
func isBadMessage(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) || errors.Is(err, jsonrpc.ErrMessageTooLarge)
}

func badMessageResponse(err error) *jsonrpc.Message {
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return jsonrpc.NewErrorResponse(nil, rpcErr)
	}
	return jsonrpc.NewErrorResponse(nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "message too large"})
}
