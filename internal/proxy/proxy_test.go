package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/fakeupstream"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

const testTimeout = 5 * time.Second

const validMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// fakeUpstream lets each test decide how the upstream behaves.
type fakeUpstream struct {
	call     func(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
	notified chan *jsonrpc.Message
}

func (f *fakeUpstream) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	return f.call(ctx, method, params)
}

func (f *fakeUpstream) Notify(_ context.Context, method string, params json.RawMessage) error {
	f.notified <- jsonrpc.NewNotification(method, params)
	return nil
}

// blockUntilCancelled is an upstream call that never answers. It reports the
// context error it saw on stopped.
func blockUntilCancelled(started chan<- struct{}, stopped chan<- error) func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
	return func(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, ctx.Err()
	}
}

// client is the test's view of an agent talking to the proxy.
type client struct {
	t         *testing.T
	in        *io.PipeWriter
	responses chan *jsonrpc.Message
	served    chan error
}

func startProxy(t *testing.T, p *Proxy, ctx context.Context) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, responses: make(chan *jsonrpc.Message, 16), served: make(chan error, 1)}

	go func() {
		c.served <- p.Serve(ctx, inR, outW)
		outW.Close()
	}()
	go func() {
		r := jsonrpc.NewReader(outR, jsonrpc.DefaultMaxMessageSize)
		for {
			msg, err := r.Read()
			if err != nil {
				close(c.responses)
				return
			}
			c.responses <- msg
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		outR.Close()
	})
	return c
}

func newClient(t *testing.T, up Upstream, opts Options) (*client, *Proxy) {
	t.Helper()
	p := New(up, opts)
	return startProxy(t, p, context.Background()), p
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatalf("client write: %v", err)
	}
}

func (c *client) next() *jsonrpc.Message {
	c.t.Helper()
	select {
	case msg, ok := <-c.responses:
		if !ok {
			c.t.Fatal("proxy output closed, want a message")
		}
		return msg
	case <-time.After(testTimeout):
		c.t.Fatal("no message from proxy in time")
		return nil
	}
}

func (c *client) waitServed() error {
	c.t.Helper()
	select {
	case err := <-c.served:
		return err
	case <-time.After(testTimeout):
		c.t.Fatal("Serve did not return in time")
		return nil
	}
}

func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestRequestIsForwardedAndClientIDRestored(t *testing.T) {
	type seen struct {
		method string
		params string
	}
	calls := make(chan seen, 1)
	up := &fakeUpstream{call: func(_ context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
		calls <- seen{method, string(params)}
		// The upstream answers with its own ID, which the client must never see.
		return jsonrpc.NewResult(jsonrpc.NewIntID(777), json.RawMessage(`{"resultType":"complete","tools":[]}`)), nil
	}}
	c, _ := newClient(t, up, Options{})

	params := `{` + validMeta + `,"cursor":"abc"}`
	c.send(`{"jsonrpc":"2.0","id":"client-1","method":"tools/list","params":` + params + `}`)

	got := wait(t, calls, "upstream call")
	if got.method != "tools/list" || got.params != params {
		t.Errorf("upstream got %q %s, want tools/list with params unchanged", got.method, got.params)
	}
	resp := c.next()
	if resp.ID == nil || *resp.ID != jsonrpc.NewStringID("client-1") {
		t.Fatalf("response id = %v, want \"client-1\"", resp.ID)
	}
	if string(resp.Result) != `{"resultType":"complete","tools":[]}` {
		t.Errorf("result = %s, want it unchanged", resp.Result)
	}
}

func TestUpstreamErrorResponsePassesThrough(t *testing.T) {
	up := &fakeUpstream{call: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
		return jsonrpc.NewErrorResponse(ptr(jsonrpc.NewIntID(9)), &jsonrpc.Error{
			Code: jsonrpc.CodeInvalidParams, Message: "Unknown tool: nope", Data: json.RawMessage(`{"x":1}`),
		}), nil
	}}
	c, _ := newClient(t, up, Options{})

	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{}}`)
	resp := c.next()

	if *resp.ID != jsonrpc.NewIntID(4) {
		t.Errorf("id = %v, want 4", resp.ID)
	}
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams || resp.Error.Message != "Unknown tool: nope" || string(resp.Error.Data) != `{"x":1}` {
		t.Errorf("error = %+v, want the upstream error unchanged", resp.Error)
	}
}

func TestCallFailures(t *testing.T) {
	tests := []struct {
		name    string
		call    func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error)
		wantMsg string
	}{
		{
			name: "timeout",
			call: func(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			wantMsg: "upstream timed out",
		},
		{
			name: "upstream gone",
			call: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
				return nil, upstream.ErrClosed
			},
			wantMsg: "upstream unavailable",
		},
		{
			name: "other errors hide their details",
			call: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
				return nil, errors.New("open /etc/secret-config: permission denied")
			},
			wantMsg: "upstream error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newClient(t, &fakeUpstream{call: tt.call}, Options{CallTimeout: 50 * time.Millisecond})
			c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)

			resp := c.next()
			if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError {
				t.Fatalf("response = %+v, want internal error", resp)
			}
			if resp.Error.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", resp.Error.Message, tt.wantMsg)
			}
			if *resp.ID != jsonrpc.NewIntID(1) {
				t.Errorf("id = %v, want 1", resp.ID)
			}
		})
	}
}

func TestClientCancelStopsCallWithoutResponse(t *testing.T) {
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	up := &fakeUpstream{notified: make(chan *jsonrpc.Message, 1)}
	up.call = func(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
		if method == "ping" {
			return jsonrpc.NewResult(jsonrpc.NewIntID(1), json.RawMessage(`{}`)), nil
		}
		return blockUntilCancelled(started, stopped)(ctx, method, params)
	}
	c, _ := newClient(t, up, Options{})

	c.send(`{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{}}`)
	wait(t, started, "slow call to start")
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow"}}`)

	if err := wait(t, stopped, "slow call to stop"); !errors.Is(err, context.Canceled) {
		t.Fatalf("upstream call saw %v, want context.Canceled", err)
	}
	// The next thing the client receives must be the ping reply, not a reply
	// to the cancelled request.
	c.send(`{"jsonrpc":"2.0","id":"ping-1","method":"ping","params":{}}`)
	if resp := c.next(); *resp.ID != jsonrpc.NewStringID("ping-1") {
		t.Fatalf("got response for %v, want only ping-1", resp.ID)
	}
	// The client's cancel names the client's ID, so it must not go upstream as-is.
	select {
	case n := <-up.notified:
		t.Errorf("upstream got notification %q, want none", n.Method)
	default:
	}
}

func TestClientNotificationsAreForwarded(t *testing.T) {
	up := &fakeUpstream{notified: make(chan *jsonrpc.Message, 1)}
	c, _ := newClient(t, up, Options{})

	c.send(`{"jsonrpc":"2.0","method":"notifications/custom","params":{"k":"v"}}`)

	n := wait(t, up.notified, "forwarded notification")
	if n.Method != "notifications/custom" || string(n.Params) != `{"k":"v"}` {
		t.Errorf("upstream got %q %s, want it unchanged", n.Method, n.Params)
	}
}

func TestBadClientMessagesGetErrorsAndProxyContinues(t *testing.T) {
	up := &fakeUpstream{call: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
		return jsonrpc.NewResult(jsonrpc.NewIntID(1), json.RawMessage(`{}`)), nil
	}}
	c, _ := newClient(t, up, Options{})

	c.send(`not json`)
	c.send(`{"jsonrpc":"2.0","id":1,"result":{}}`) // clients must not send responses; ignored
	c.send(`{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`)

	first := c.next()
	if first.Error == nil || first.Error.Code != jsonrpc.CodeParseError || first.ID != nil {
		t.Errorf("first = %+v, want parse error with null id", first)
	}
	if second := c.next(); *second.ID != jsonrpc.NewIntID(2) || second.Error != nil {
		t.Errorf("second = %+v, want result for id 2", second)
	}
}

func TestDuplicateInFlightIDIsRejected(t *testing.T) {
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	c, _ := newClient(t, &fakeUpstream{call: blockUntilCancelled(started, stopped)}, Options{})

	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)
	wait(t, started, "first call to start")
	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`)

	resp := c.next()
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidRequest || *resp.ID != jsonrpc.NewIntID(5) {
		t.Fatalf("response = %+v, want invalid request for id 5", resp)
	}
}

func TestSlowCallDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	up := &fakeUpstream{call: func(_ context.Context, method string, _ json.RawMessage) (*jsonrpc.Message, error) {
		if method == "slow" {
			<-release
		}
		return jsonrpc.NewResult(jsonrpc.NewIntID(1), json.RawMessage(`{}`)), nil
	}}
	c, _ := newClient(t, up, Options{})

	c.send(`{"jsonrpc":"2.0","id":"a","method":"slow","params":{}}`)
	c.send(`{"jsonrpc":"2.0","id":"b","method":"fast","params":{}}`)

	if resp := c.next(); *resp.ID != jsonrpc.NewStringID("b") {
		t.Fatalf("first response id = %v, want \"b\"", resp.ID)
	}
	close(release)
	if resp := c.next(); *resp.ID != jsonrpc.NewStringID("a") {
		t.Fatalf("second response id = %v, want \"a\"", resp.ID)
	}
}

// A client may close its input right after sending, as `echo req | gateway`
// does, and still read the replies.
func TestClientEOFWaitsForInFlightReplies(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	up := &fakeUpstream{call: func(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
		started <- struct{}{}
		select {
		case <-release:
			return jsonrpc.NewResult(jsonrpc.NewIntID(99), json.RawMessage(`{"ok":true}`)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	c, _ := newClient(t, up, Options{})

	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	wait(t, started, "call to start")
	c.in.Close()

	select {
	case err := <-c.served:
		t.Fatalf("Serve() returned %v right after EOF, want it to wait for the call", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	if resp := c.next(); resp.Error != nil || *resp.ID != jsonrpc.NewIntID(1) || string(resp.Result) != `{"ok":true}` {
		t.Fatalf("response = %+v, want the result for id 1", resp)
	}
	if err := c.waitServed(); err != nil {
		t.Fatalf("Serve() error = %v, want nil on client EOF", err)
	}
}

func TestClientEOFStillAppliesCallTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	c, _ := newClient(t, &fakeUpstream{call: blockUntilCancelled(started, stopped)}, Options{CallTimeout: 50 * time.Millisecond})

	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	wait(t, started, "call to start")
	c.in.Close()

	if resp := c.next(); resp.Error == nil || resp.Error.Message != "upstream timed out" {
		t.Fatalf("response = %+v, want upstream timed out", resp)
	}
	if err := c.waitServed(); err != nil {
		t.Fatalf("Serve() error = %v, want nil on client EOF", err)
	}
}

func TestContextCancelAfterEOFStopsInFlight(t *testing.T) {
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	c := startProxy(t, New(&fakeUpstream{call: blockUntilCancelled(started, stopped)}, Options{}), ctx)

	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	wait(t, started, "call to start")
	c.in.Close()
	cancel()

	if err := c.waitServed(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve() error = %v, want context.Canceled", err)
	}
	if err := wait(t, stopped, "call to stop"); !errors.Is(err, context.Canceled) {
		t.Errorf("in-flight call saw %v, want context.Canceled", err)
	}
}

func TestContextCancelStopsServe(t *testing.T) {
	p := New(&fakeUpstream{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	c := startProxy(t, p, ctx)

	cancel()
	if err := c.waitServed(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve() error = %v, want context.Canceled", err)
	}
}

func TestUpstreamNotificationsReachClient(t *testing.T) {
	up := &fakeUpstream{call: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
		return jsonrpc.NewResult(jsonrpc.NewIntID(1), json.RawMessage(`{}`)), nil
	}}
	c, p := newClient(t, up, Options{})

	// A round trip first, so Serve is surely running and has its writer set.
	c.send(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`)
	c.next()

	p.ForwardNotification(jsonrpc.NewNotification("notifications/progress", json.RawMessage(`{"progressToken":"t","progress":1}`)))

	msg := c.next()
	if msg.Kind() != jsonrpc.KindNotification || msg.Method != "notifications/progress" {
		t.Fatalf("client got %v %q, want notifications/progress", msg.Kind(), msg.Method)
	}
	if string(msg.Params) != `{"progressToken":"t","progress":1}` {
		t.Errorf("params = %s, want them unchanged", msg.Params)
	}
}

func TestEndToEndWithFakeUpstream(t *testing.T) {
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	go func() {
		fromServerW.CloseWithError(fakeupstream.Serve(toServerR, fromServerW, jsonrpc.DefaultMaxMessageSize))
	}()
	conn := upstream.NewConn(fromServerR, toServerW, upstream.Options{})
	t.Cleanup(func() {
		toServerW.Close()
		<-conn.Done()
	})
	c, _ := newClient(t, conn, Options{})

	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	list := c.next()
	if list.Error != nil || !strings.Contains(string(list.Result), `"name":"echo"`) {
		t.Fatalf("tools/list = %s / %v", list.Result, list.Error)
	}

	c.send(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{` + validMeta + `,"name":"echo","arguments":{"text":"through the gateway"}}}`)
	call := c.next()
	if *call.ID != jsonrpc.NewStringID("call-1") || !strings.Contains(string(call.Result), "through the gateway") {
		t.Fatalf("tools/call = %v / %s / %v", call.ID, call.Result, call.Error)
	}

	// Missing _meta must reach the upstream as-is and come back as its error.
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	if resp := c.next(); resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("response = %+v, want the upstream's invalid params error", resp)
	}
}

func ptr[T any](v T) *T { return &v }
