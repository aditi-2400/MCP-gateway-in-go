// Package progress routes upstream progress notifications back to the client
// request that asked for them.
//
// A client asks for progress by putting a progressToken in a request's _meta.
// Tokens are chosen by clients, so two HTTP clients can pick the same one.
// The gateway therefore hands upstreams its own unique token and maps it back
// when a notification arrives, the same way it rewrites request IDs.
package progress

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const notificationMethod = "notifications/progress"

// tokenPrefix makes gateway tokens easy to spot in upstream logs.
const tokenPrefix = "gateway-progress-"

// Tracker maps gateway progress tokens to the requests waiting for them. It
// is safe for concurrent use.
type Tracker struct {
	next atomic.Uint64

	mu      sync.Mutex
	waiting map[string]waiter // gateway token -> request
}

type waiter struct {
	original json.RawMessage // the client's token, restored on delivery
	send     func(*jsonrpc.Message)
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{waiting: make(map[string]waiter)}
}

// Track looks for _meta.progressToken in a request's params. If there is one,
// it returns params with the token replaced by a new gateway token, and
// notifications for that token go to send until release is called. Without a
// token, or if params are not an object, params come back unchanged and
// tracked is false. release is always safe to call.
func (t *Tracker) Track(params json.RawMessage, send func(*jsonrpc.Message)) (out json.RawMessage, release func(), tracked bool) {
	noop := func() {}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil || fields == nil {
		return params, noop, false
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(fields["_meta"], &meta); err != nil || meta == nil {
		return params, noop, false
	}
	original, ok := meta["progressToken"]
	if !ok || string(original) == "null" {
		return params, noop, false
	}

	token := tokenPrefix + strconv.FormatUint(t.next.Add(1), 10)
	encoded, err := json.Marshal(token)
	if err != nil {
		return params, noop, false
	}
	meta["progressToken"] = encoded
	if fields["_meta"], err = json.Marshal(meta); err != nil {
		return params, noop, false
	}
	if out, err = json.Marshal(fields); err != nil {
		return params, noop, false
	}

	t.mu.Lock()
	t.waiting[token] = waiter{original: original, send: send}
	t.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.waiting, token)
			t.mu.Unlock()
		})
	}
	return out, release, true
}

// Deliver handles one upstream notification. It returns false if msg is not
// a progress notification, so the caller can handle it another way. Progress
// for a token that is unknown or already released is dropped: the request it
// belonged to has finished, and the spec forbids progress after that.
func (t *Tracker) Deliver(msg *jsonrpc.Message) bool {
	if msg.Method != notificationMethod {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(msg.Params, &fields); err != nil || fields == nil {
		return true
	}
	var token string
	if err := json.Unmarshal(fields["progressToken"], &token); err != nil {
		return true
	}

	t.mu.Lock()
	w, ok := t.waiting[token]
	t.mu.Unlock()
	if !ok {
		return true
	}

	fields["progressToken"] = w.original
	params, err := json.Marshal(fields)
	if err != nil {
		return true
	}
	w.send(jsonrpc.NewNotification(notificationMethod, params))
	return true
}

// Strip removes progressToken from a _meta object. The router uses it when
// one client request fans out to several upstreams, because their combined
// progress would not be one steadily increasing count as the spec requires.
func Strip(meta json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(meta, &fields); err != nil || fields == nil {
		return meta
	}
	if _, ok := fields["progressToken"]; !ok {
		return meta
	}
	delete(fields, "progressToken")
	out, err := json.Marshal(fields)
	if err != nil {
		return meta
	}
	return out
}

// Stream orders the messages for one request: progress notifications first,
// then the final reply, then nothing. Progress can arrive on an upstream's
// read loop at the same moment the reply is being written, and a
// notification after the reply would break the spec.
type Stream struct {
	mu   sync.Mutex
	done bool
	send func(*jsonrpc.Message)
}

// NewStream returns a Stream that writes notifications with send.
func NewStream(send func(*jsonrpc.Message)) *Stream {
	return &Stream{send: send}
}

// Send writes a notification unless the stream is finished.
func (s *Stream) Send(msg *jsonrpc.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		s.send(msg)
	}
}

// Finish runs final (the reply write) and drops every later Send.
func (s *Stream) Finish(final func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	if final != nil {
		final()
	}
}
