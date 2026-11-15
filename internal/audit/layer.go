package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit/note"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// maxStoredArguments caps the redacted arguments kept per entry. Bigger
// ones are replaced by a marker with their size, so one huge call can't
// bloat the log.
const maxStoredArguments = 64 << 10

// maxStoredToolName caps the tool name kept per entry. Names come from the
// client, and unknown ones are audited too.
const maxStoredToolName = 256

// Handler answers client requests, with the same shape as proxy.Handler.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
	Notify(ctx context.Context, method string, params json.RawMessage) error
}

// Layer is the outermost layer for tools/call. It reserves an audit slot
// before anything else happens, so a call that can't be recorded is
// refused, then lets the call run and commits one entry for it, whatever
// the outcome.
type Layer struct {
	next   Handler
	rec    *Recorder
	logger *slog.Logger
	now    func() time.Time
	obs    Observer
}

// UnavailableMessage is the error message of a call refused because the
// audit log had no room for it.
const UnavailableMessage = "audit log unavailable"

// Observer hears about every audited call, for metrics. It is called on the
// request's goroutine, so it must be quick and must not block.
type Observer interface {
	// CallDone gets the finished entry and the call's total time in the
	// gateway.
	CallDone(e Entry, total time.Duration)
	// CallRefused is told about a call refused because the audit log had no
	// room. Such calls have no entry.
	CallRefused()
}

type noObserver struct{}

func (noObserver) CallDone(Entry, time.Duration) {}
func (noObserver) CallRefused()                  {}

// NewLayer returns a Layer that records calls passing to next.
func NewLayer(next Handler, rec *Recorder, logger *slog.Logger) *Layer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Layer{next: next, rec: rec, logger: logger, now: time.Now, obs: noObserver{}}
}

// SetObserver sets who hears about calls. Call it before the layer serves.
func (l *Layer) SetObserver(o Observer) {
	l.obs = o
}

// Call audits tools/call and passes everything else on.
func (l *Layer) Call(ctx context.Context, method string, params json.RawMessage) (resp *jsonrpc.Message, err error) {
	if method != "tools/call" {
		return l.next.Call(ctx, method, params)
	}
	slot, err := l.rec.Reserve(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // the client gave up; nothing to answer
		}
		l.logger.Error("refusing tools/call: audit log unavailable", "error", err)
		l.obs.CallRefused()
		return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: UnavailableMessage}}, nil
	}

	agent, _ := auth.FromContext(ctx)
	tool, args := readCall(params)
	entry := Entry{
		Time:      l.now(),
		RequestID: rand.Text(),
		Agent:     agent.Name,
		Tool:      tool,
		Arguments: storedArguments(args),
	}
	ctx, n := note.Ensure(ctx)
	// Deferred, so the slot is committed even if a layer below panics;
	// an uncommitted slot would never be freed.
	defer func() {
		fillOutcome(&entry, n.Get(), resp, err)
		slot.Commit(entry)
		l.obs.CallDone(entry, l.now().Sub(entry.Time))
	}()
	return l.next.Call(ctx, method, params)
}

// Notify passes notifications on; they are not audited.
func (l *Layer) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return l.next.Notify(ctx, method, params)
}

// readCall gets the tool name and arguments, as far as they can be read.
// The policy guard rejects malformed calls; they are still audited.
func readCall(params json.RawMessage) (tool string, args json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", nil
	}
	if len(p.Name) > maxStoredToolName {
		// Cutting by bytes can split a character; drop the broken piece.
		p.Name = strings.ToValidUTF8(p.Name[:maxStoredToolName], "")
	}
	return p.Name, p.Arguments
}

func storedArguments(args json.RawMessage) json.RawMessage {
	redacted := RedactArguments(args)
	if len(redacted) > maxStoredArguments {
		return json.RawMessage(fmt.Sprintf(`{"[truncated]":true,"bytes":%d}`, len(redacted)))
	}
	return redacted
}

// fillOutcome sets the decision from the note and the status from the reply.
func fillOutcome(e *Entry, s note.Snapshot, resp *jsonrpc.Message, err error) {
	e.Decision, e.Reason, e.Rule, e.Latency = Decision(s.Decision), s.Reason, s.Rule, s.Latency
	if e.Decision == "" {
		// No layer said what it did. If the call got through anyway, record
		// that; if not, it was stopped before the policy ran.
		if s.Forwarded {
			e.Decision, e.Reason = Allowed, "forwarded with no policy decision recorded"
		} else {
			e.Decision, e.Reason = Denied, "stopped before a policy decision"
		}
	}
	switch {
	case err != nil || resp == nil:
		e.Status = StatusNoResponse
	case resp.Error != nil:
		e.Status, e.ErrorCode = StatusRPCError, resp.Error.Code
	default:
		var r struct {
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(resp.Result, &r) == nil && r.IsError {
			e.Status = StatusToolError
		} else {
			e.Status = StatusOK
		}
	}
}

// UpstreamTimer sits right in front of the router and notes how long each
// tools/call spends there. A call that reaches it passed every gateway
// check.
type UpstreamTimer struct {
	next Handler
	now  func() time.Time
}

// TimeUpstream returns an UpstreamTimer in front of next.
func TimeUpstream(next Handler) *UpstreamTimer {
	return &UpstreamTimer{next: next, now: time.Now}
}

// Call times tools/call and passes everything on.
func (u *UpstreamTimer) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	if method != "tools/call" {
		return u.next.Call(ctx, method, params)
	}
	start := u.now()
	defer func() { note.Upstream(ctx, u.now().Sub(start)) }()
	return u.next.Call(ctx, method, params)
}

// Notify passes notifications on.
func (u *UpstreamTimer) Notify(ctx context.Context, method string, params json.RawMessage) error {
	return u.next.Notify(ctx, method, params)
}
