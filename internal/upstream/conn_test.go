package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/fakeupstream"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const testTimeout = 5 * time.Second

const validParams = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`

func echoParams(text string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"echo","arguments":{"text":%q}}`,
		text))
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// newFakeConn connects a Conn to the real fake upstream running in a goroutine.
func newFakeConn(t *testing.T) *Conn {
	t.Helper()
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	go func() {
		err := fakeupstream.Serve(toServerR, fromServerW, jsonrpc.DefaultMaxMessageSize)
		fromServerW.CloseWithError(err)
	}()
	conn := NewConn(fromServerR, toServerW, Options{})
	t.Cleanup(func() {
		toServerW.Close()
		<-conn.Done()
	})
	return conn
}

// scriptedUpstream lets a test play the server by hand: read what the Conn
// sent and write back exactly the lines the test wants.
type scriptedUpstream struct {
	t        *testing.T
	requests *jsonrpc.Reader
	toConn   *io.PipeWriter
}

func newScriptedConn(t *testing.T, opts Options) (*Conn, *scriptedUpstream) {
	t.Helper()
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	conn := NewConn(fromServerR, toServerW, opts)
	up := &scriptedUpstream{
		t:        t,
		requests: jsonrpc.NewReader(toServerR, jsonrpc.DefaultMaxMessageSize),
		toConn:   fromServerW,
	}
	t.Cleanup(func() {
		fromServerW.Close()
		toServerR.Close()
		<-conn.Done()
	})
	return conn, up
}

func (s *scriptedUpstream) read() *jsonrpc.Message {
	s.t.Helper()
	msg, err := s.requests.Read()
	if err != nil {
		s.t.Fatalf("scripted upstream read: %v", err)
	}
	return msg
}

func (s *scriptedUpstream) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.toConn, line+"\n"); err != nil {
		s.t.Fatalf("scripted upstream write: %v", err)
	}
}

func (s *scriptedUpstream) reply(id jsonrpc.ID, result string) {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":` + id.String() + `,"result":` + result + `}`)
}

type callOutcome struct {
	resp *jsonrpc.Message
	err  error
}

// callAsync runs Call in a goroutine so the test can play the upstream meanwhile.
func callAsync(ctx context.Context, conn *Conn, method string) <-chan callOutcome {
	out := make(chan callOutcome, 1)
	go func() {
		resp, err := conn.Call(ctx, method, json.RawMessage(validParams))
		out <- callOutcome{resp, err}
	}()
	return out
}

func waitOutcome(t *testing.T, ch <-chan callOutcome) callOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(testTimeout):
		t.Fatal("Call did not return in time")
		return callOutcome{}
	}
}

func pendingCount(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func TestCallReturnsMatchingResponse(t *testing.T) {
	conn, up := newScriptedConn(t, Options{})
	outcome := callAsync(testContext(t), conn, "tools/list")

	req := up.read()
	if req.Kind() != jsonrpc.KindRequest || req.Method != "tools/list" {
		t.Fatalf("upstream got %v %q, want request tools/list", req.Kind(), req.Method)
	}
	if string(req.Params) != validParams {
		t.Errorf("params = %s, want them forwarded unchanged", req.Params)
	}
	up.reply(*req.ID, `{"resultType":"complete","tools":[]}`)

	o := waitOutcome(t, outcome)
	if o.err != nil {
		t.Fatalf("Call() error = %v", o.err)
	}
	if string(o.resp.Result) != `{"resultType":"complete","tools":[]}` {
		t.Errorf("result = %s", o.resp.Result)
	}
	if n := pendingCount(conn); n != 0 {
		t.Errorf("pending has %d entries after the call, want 0", n)
	}
}

func TestConcurrentCallsGetTheirOwnResponses(t *testing.T) {
	conn := newFakeConn(t)
	ctx := testContext(t)
	const calls = 50

	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("message %d", i)
			resp, err := conn.Call(ctx, "tools/call", echoParams(want))
			if err != nil {
				t.Errorf("Call(%d) error = %v", i, err)
				return
			}
			var result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(resp.Result, &result); err != nil || len(result.Content) != 1 {
				t.Errorf("Call(%d) result = %s", i, resp.Result)
				return
			}
			if got := result.Content[0].Text; got != want {
				t.Errorf("Call(%d) got %q, want %q (responses were mixed up)", i, got, want)
			}
		}()
	}
	wg.Wait()
}

func TestUpstreamErrorResponseIsReturnedAsMessage(t *testing.T) {
	conn := newFakeConn(t)
	params := json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"nope"}`)

	resp, err := conn.Call(testContext(t), "tools/call", params)
	if err != nil {
		t.Fatalf("Call() error = %v, want nil (the upstream answered)", err)
	}
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("response error = %v, want code %d", resp.Error, jsonrpc.CodeInvalidParams)
	}
}

func TestCancelledCallSendsCancelNotification(t *testing.T) {
	conn, up := newScriptedConn(t, Options{})
	ctx, cancel := context.WithCancel(testContext(t))
	outcome := callAsync(ctx, conn, "tools/call")

	req := up.read()
	cancel()

	o := waitOutcome(t, outcome)
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("Call() error = %v, want context.Canceled", o.err)
	}

	note := up.read()
	if note.Kind() != jsonrpc.KindNotification || note.Method != "notifications/cancelled" {
		t.Fatalf("upstream got %v %q, want notifications/cancelled", note.Kind(), note.Method)
	}
	var params struct {
		RequestID jsonrpc.ID `json:"requestId"`
	}
	if err := json.Unmarshal(note.Params, &params); err != nil {
		t.Fatalf("decode cancel params %s: %v", note.Params, err)
	}
	if params.RequestID != *req.ID {
		t.Errorf("cancelled requestId = %v, want %v", params.RequestID, *req.ID)
	}
	if n := pendingCount(conn); n != 0 {
		t.Errorf("pending has %d entries after cancel, want 0", n)
	}

	// A late reply to the cancelled request is dropped, and the next call still works.
	up.reply(*req.ID, `{"late":true}`)
	next := callAsync(testContext(t), conn, "tools/list")
	req2 := up.read()
	up.reply(*req2.ID, `{"second":true}`)
	if o := waitOutcome(t, next); o.err != nil || string(o.resp.Result) != `{"second":true}` {
		t.Fatalf("next Call() = %v, %v; want the second result", o.resp, o.err)
	}
}

func TestCallWithDoneContextSendsNothing(t *testing.T) {
	conn, _ := newScriptedConn(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// If Call tried to write, it would block forever on the unread pipe.
	if _, err := conn.Call(ctx, "tools/list", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() error = %v, want context.Canceled", err)
	}
}

func TestUpstreamClosingFailsPendingAndLaterCalls(t *testing.T) {
	conn, up := newScriptedConn(t, Options{})
	outcome := callAsync(testContext(t), conn, "tools/call")

	up.read()
	up.toConn.Close() // upstream stdout ends, as if the process died

	if o := waitOutcome(t, outcome); !errors.Is(o.err, ErrClosed) {
		t.Fatalf("pending Call() error = %v, want ErrClosed", o.err)
	}
	if _, err := conn.Call(testContext(t), "tools/list", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("later Call() error = %v, want ErrClosed", err)
	}
	if err := conn.Notify(testContext(t), "notifications/x", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Notify() error = %v, want ErrClosed", err)
	}
}

func TestUpstreamReadErrorIsKept(t *testing.T) {
	conn, up := newScriptedConn(t, Options{})
	boom := errors.New("pipe broke")
	up.toConn.CloseWithError(boom)
	<-conn.Done()

	if err := conn.Err(); !errors.Is(err, ErrClosed) || !errors.Is(err, boom) {
		t.Fatalf("Err() = %v, want ErrClosed wrapping %v", err, boom)
	}
}

func TestNotificationsFromUpstreamAreDelivered(t *testing.T) {
	got := make(chan *jsonrpc.Message, 1)
	_, up := newScriptedConn(t, Options{OnNotification: func(m *jsonrpc.Message) { got <- m }})

	up.send(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"t1","progress":50}}`)

	select {
	case m := <-got:
		if m.Method != "notifications/progress" {
			t.Errorf("Method = %q, want notifications/progress", m.Method)
		}
	case <-time.After(testTimeout):
		t.Fatal("notification was not delivered")
	}
}

func TestBadUpstreamMessagesAreSkipped(t *testing.T) {
	conn, up := newScriptedConn(t, Options{})
	outcome := callAsync(testContext(t), conn, "tools/list")
	req := up.read()

	up.send(`this is not json`)
	up.send(`{"jsonrpc":"2.0","id":999,"result":{}}`)                            // nobody is waiting for 999
	up.send(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"x"}}`) // cannot be routed
	up.send(`{"jsonrpc":"2.0","id":5,"method":"sampling/createMessage","params":{}}`)
	up.reply(*req.ID, `{"ok":true}`)

	o := waitOutcome(t, outcome)
	if o.err != nil || string(o.resp.Result) != `{"ok":true}` {
		t.Fatalf("Call() = %v, %v; want the real response", o.resp, o.err)
	}
}

func TestNotifySendsNotification(t *testing.T) {
	conn, up := newScriptedConn(t, Options{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Notify(testContext(t), "notifications/cancelled", json.RawMessage(`{"requestId":3}`))
	}()

	msg := up.read()
	if msg.Kind() != jsonrpc.KindNotification || msg.Method != "notifications/cancelled" {
		t.Fatalf("upstream got %v %q, want notification", msg.Kind(), msg.Method)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
}
