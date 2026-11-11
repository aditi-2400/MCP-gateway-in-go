package httpfront

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/progress"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

const testTimeout = 5 * time.Second

const validMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// fakeHandler stands in for the router.
type fakeHandler struct {
	mu       sync.Mutex
	calls    []string
	notified []string
	answer   func(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
}

func (f *fakeHandler) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	if f.answer == nil {
		return &jsonrpc.Message{Result: json.RawMessage(`{"ok":true}`)}, nil
	}
	return f.answer(ctx, method, params)
}

func (f *fakeHandler) Notify(_ context.Context, method string, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, method)
	return nil
}

func (f *fakeHandler) notifiedMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notified...)
}

func (f *fakeHandler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func answerError(rpcErr *jsonrpc.Error) func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
	return func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
		return &jsonrpc.Message{Error: rpcErr}, nil
	}
}

func newServer(t *testing.T, h *fakeHandler, opts Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(h, opts))
	t.Cleanup(srv.Close)
	return srv
}

// post sends body with the standard MCP headers for method, plus extra.
// A header set to "" in extra is removed.
func post(t *testing.T, srv *httptest.Server, method, body string, extra map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	for k, v := range extra {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func request(id, method, params string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":{` + params + `}}`
}

// decode reads a JSON-RPC response body.
func decode(t *testing.T, resp *http.Response) *jsonrpc.Message {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := jsonrpc.Parse(body)
	if err != nil {
		t.Fatalf("parse response %s: %v", body, err)
	}
	return msg
}

func wantStatus(t *testing.T, resp *http.Response, status int) {
	t.Helper()
	if resp.StatusCode != status {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, status, body)
	}
}

func wantRPCError(t *testing.T, resp *http.Response, status, code int) *jsonrpc.Message {
	t.Helper()
	wantStatus(t, resp, status)
	msg := decode(t, resp)
	if msg.Error == nil || msg.Error.Code != code {
		t.Fatalf("response = result %s, error %v, want code %d", msg.Result, msg.Error, code)
	}
	return msg
}

func TestRequestIsAnsweredWithClientID(t *testing.T) {
	h := &fakeHandler{}
	srv := newServer(t, h, Options{})

	resp := post(t, srv, "tools/list", request(`"abc"`, "tools/list", validMeta), nil)
	wantStatus(t, resp, http.StatusOK)
	msg := decode(t, resp)
	if msg.ID == nil || *msg.ID != jsonrpc.NewStringID("abc") || string(msg.Result) != `{"ok":true}` {
		t.Fatalf("response = id %v, result %s, want id \"abc\" and the handler's result", msg.ID, msg.Result)
	}
}

func TestOnlyPostIsAllowed(t *testing.T) {
	srv := newServer(t, &fakeHandler{}, Options{})
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, srv.URL+Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			wantStatus(t, resp, http.StatusMethodNotAllowed)
			if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
				t.Errorf("Allow = %q, want POST", allow)
			}
		})
	}
}

func TestOriginCheck(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		origin  string
		want    int
	}{
		{name: "no origin header", origin: "", want: http.StatusOK},
		{name: "origin with empty allowlist", origin: "http://localhost:3000", want: http.StatusForbidden},
		{name: "allowed origin", allowed: []string{"http://localhost:3000"}, origin: "http://localhost:3000", want: http.StatusOK},
		{name: "other origin", allowed: []string{"http://localhost:3000"}, origin: "http://evil.example", want: http.StatusForbidden},
		{name: "different port", allowed: []string{"http://localhost:3000"}, origin: "http://localhost:3001", want: http.StatusForbidden},
		{name: "null origin", allowed: []string{"http://localhost:3000"}, origin: "null", want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			srv := newServer(t, h, Options{AllowedOrigins: tt.allowed})
			resp := post(t, srv, "tools/list", request("1", "tools/list", validMeta), map[string]string{"Origin": tt.origin})
			wantStatus(t, resp, tt.want)
			if tt.want == http.StatusForbidden && h.callCount() != 0 {
				t.Errorf("handler called %d times for a rejected origin", h.callCount())
			}
		})
	}
}

func TestBodyChecks(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    int // 0 means no JSON-RPC body is checked
	}{
		{name: "no content type", contentType: "", body: request("1", "tools/list", validMeta), wantStatus: http.StatusUnsupportedMediaType},
		{name: "text content type", contentType: "text/plain", body: request("1", "tools/list", validMeta), wantStatus: http.StatusUnsupportedMediaType},
		{name: "json with charset", contentType: "application/json; charset=utf-8", body: request("1", "tools/list", validMeta), wantStatus: http.StatusOK},
		{name: "too large", contentType: "application/json", body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("x", 2048) + `"}}`, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "not json", contentType: "application/json", body: `{"jsonrpc":`, wantStatus: http.StatusBadRequest, wantCode: jsonrpc.CodeParseError},
		{name: "batch", contentType: "application/json", body: `[` + request("1", "tools/list", validMeta) + `]`, wantStatus: http.StatusBadRequest, wantCode: jsonrpc.CodeInvalidRequest},
		{name: "client sends a response", contentType: "application/json", body: `{"jsonrpc":"2.0","id":1,"result":{}}`, wantStatus: http.StatusBadRequest, wantCode: jsonrpc.CodeInvalidRequest},
		{name: "empty body", contentType: "application/json", body: ``, wantStatus: http.StatusBadRequest, wantCode: jsonrpc.CodeParseError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			srv := newServer(t, h, Options{MaxMessageSize: 1024})
			resp := post(t, srv, "tools/list", tt.body, map[string]string{"Content-Type": tt.contentType})
			if tt.wantCode != 0 {
				msg := wantRPCError(t, resp, tt.wantStatus, tt.wantCode)
				if msg.ID != nil {
					t.Errorf("id = %v, want null", msg.ID)
				}
			} else {
				wantStatus(t, resp, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK && h.callCount() != 0 {
				t.Errorf("handler called %d times for a rejected body", h.callCount())
			}
		})
	}
}

func TestNotificationIsAccepted(t *testing.T) {
	h := &fakeHandler{}
	srv := newServer(t, h, Options{})

	resp := post(t, srv, "notifications/progress", `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`, nil)
	wantStatus(t, resp, http.StatusAccepted)
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
	if got := h.notifiedMethods(); len(got) != 1 || got[0] != "notifications/progress" {
		t.Errorf("notified = %v, want [notifications/progress]", got)
	}
}

func b64(s string) string {
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

func TestHeaderValidation(t *testing.T) {
	call := request("1", "tools/call", `"name":"fs__read_file",`+validMeta)
	list := request("1", "tools/list", validMeta)
	oldVersion := request("1", "tools/list", `"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}`)

	tests := []struct {
		name    string
		method  string // value for the Mcp-Method header
		body    string
		headers map[string]string
		ok      bool
	}{
		{name: "all headers match", method: "tools/list", body: list, ok: true},
		{name: "tools/call with Mcp-Name", method: "tools/call", body: call, headers: map[string]string{"Mcp-Name": "fs__read_file"}, ok: true},
		{name: "base64 Mcp-Name", method: "tools/call", body: call, headers: map[string]string{"Mcp-Name": b64("fs__read_file")}, ok: true},
		{name: "missing protocol version", method: "tools/list", body: list, headers: map[string]string{"MCP-Protocol-Version": ""}},
		{name: "protocol version differs from body", method: "tools/list", body: oldVersion},
		{name: "missing Mcp-Method", method: "", body: list},
		{name: "Mcp-Method differs", method: "tools/call", body: list},
		{name: "Mcp-Method differs in case", method: "TOOLS/LIST", body: list},
		{name: "tools/call without Mcp-Name", method: "tools/call", body: call},
		{name: "Mcp-Name differs", method: "tools/call", body: call, headers: map[string]string{"Mcp-Name": "fs__write_file"}},
		{name: "base64 Mcp-Name differs", method: "tools/call", body: call, headers: map[string]string{"Mcp-Name": b64("fs__write_file")}},
		{name: "broken base64", method: "tools/call", body: call, headers: map[string]string{"Mcp-Name": "=?base64?***?="}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			srv := newServer(t, h, Options{})
			resp := post(t, srv, tt.method, tt.body, tt.headers)
			if tt.ok {
				wantStatus(t, resp, http.StatusOK)
				return
			}
			msg := wantRPCError(t, resp, http.StatusBadRequest, mcp.CodeHeaderMismatch)
			if msg.ID == nil || *msg.ID != jsonrpc.NewIntID(1) {
				t.Errorf("id = %v, want 1", msg.ID)
			}
			if h.callCount() != 0 {
				t.Errorf("handler called %d times despite a header mismatch", h.callCount())
			}
		})
	}
}

func TestStatusCodesFollowErrors(t *testing.T) {
	tests := []struct {
		name   string
		answer func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error)
		status int
		code   int
	}{
		{name: "method not found", answer: answerError(&jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found"}), status: http.StatusNotFound, code: jsonrpc.CodeMethodNotFound},
		{name: "unsupported version", answer: answerError(&jsonrpc.Error{Code: mcp.CodeUnsupportedProtocolVersion, Message: "Unsupported protocol version"}), status: http.StatusBadRequest, code: mcp.CodeUnsupportedProtocolVersion},
		{name: "invalid params", answer: answerError(&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Unknown tool: x"}), status: http.StatusOK, code: jsonrpc.CodeInvalidParams},
		{name: "upstream gone", answer: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
			return nil, upstream.ErrClosed
		}, status: http.StatusOK, code: jsonrpc.CodeInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, &fakeHandler{answer: tt.answer}, Options{})
			wantRPCError(t, post(t, srv, "tools/list", request("1", "tools/list", validMeta), nil), tt.status, tt.code)
		})
	}
}

func TestCallTimeoutApplies(t *testing.T) {
	h := &fakeHandler{answer: func(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	srv := newServer(t, h, Options{CallTimeout: 50 * time.Millisecond})

	msg := wantRPCError(t, post(t, srv, "tools/list", request("1", "tools/list", validMeta), nil), http.StatusOK, jsonrpc.CodeInternalError)
	if msg.Error.Message != "upstream timed out" {
		t.Errorf("message = %q, want upstream timed out", msg.Error.Message)
	}
}

func TestClientDisconnectCancelsCall(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan error, 1)
	h := &fakeHandler{answer: func(ctx context.Context, _ string, _ json.RawMessage) (*jsonrpc.Message, error) {
		close(started)
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, ctx.Err()
	}}
	srv := newServer(t, h, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+Path, strings.NewReader(request("1", "tools/list", validMeta)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/list")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := srv.Client().Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("call did not start")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != context.Canceled {
			t.Fatalf("handler saw %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("disconnecting did not cancel the call")
	}
	<-done
}

func TestEachPostIsIndependent(t *testing.T) {
	// Two requests with the same JSON-RPC id at the same time both succeed:
	// there is no session that could make them collide.
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(2)
	h := &fakeHandler{answer: func(context.Context, string, json.RawMessage) (*jsonrpc.Message, error) {
		started.Done()
		<-release
		return &jsonrpc.Message{Result: json.RawMessage(`{}`)}, nil
	}}
	srv := newServer(t, h, Options{})

	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(request("1", "tools/list", validMeta)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
			req.Header.Set("Mcp-Method", "tools/list")
			resp, err := srv.Client().Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	started.Wait()
	close(release)
	for range 2 {
		select {
		case status := <-statuses:
			if status != http.StatusOK {
				t.Errorf("status = %d, want 200", status)
			}
		case <-time.After(testTimeout):
			t.Fatal("request did not finish")
		}
	}
}

func TestSessionIDIsIgnored(t *testing.T) {
	srv := newServer(t, &fakeHandler{}, Options{})
	resp := post(t, srv, "tools/list", request("1", "tools/list", validMeta), map[string]string{"Mcp-Session-Id": "old-client-session"})
	wantStatus(t, resp, http.StatusOK)
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.Errorf("Mcp-Session-Id = %q, want none", id)
	}
}

// progressHandler acts like an upstream that sends n progress notifications
// through tracker before answering.
func progressHandler(tracker *progress.Tracker, n int, answer *jsonrpc.Message) *fakeHandler {
	return &fakeHandler{answer: func(_ context.Context, _ string, params json.RawMessage) (*jsonrpc.Message, error) {
		var p struct {
			Meta struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		for i := 1; i <= n; i++ {
			tracker.Deliver(jsonrpc.NewNotification("notifications/progress", json.RawMessage(
				`{"progressToken":`+string(p.Meta.ProgressToken)+`,"progress":`+strconv.Itoa(i)+`}`)))
		}
		return answer, nil
	}}
}

// readEvents parses an SSE body into JSON-RPC messages.
func readEvents(t *testing.T, resp *http.Response) []*jsonrpc.Message {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []*jsonrpc.Message
	for _, block := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		var data string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data += rest
			}
		}
		msg, err := jsonrpc.Parse([]byte(data))
		if err != nil {
			t.Fatalf("parse event %q: %v", block, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func withProgress(id, token string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/list","params":{"_meta":{"progressToken":` + token +
		`,"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
}

func TestProgressIsStreamedAsSSE(t *testing.T) {
	tracker := progress.NewTracker()
	h := progressHandler(tracker, 2, &jsonrpc.Message{Result: json.RawMessage(`{"done":true}`)})
	srv := newServer(t, h, Options{Progress: tracker})

	resp := post(t, srv, "tools/list", withProgress("9", `"p-1"`), nil)
	wantStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", resp.Header.Get("X-Accel-Buffering"))
	}
	msgs := readEvents(t, resp)
	if len(msgs) != 3 {
		t.Fatalf("got %d events, want 2 progress and 1 result", len(msgs))
	}
	for i, msg := range msgs[:2] {
		want := `{"progress":` + strconv.Itoa(i+1) + `,"progressToken":"p-1"}`
		if msg.Method != "notifications/progress" || string(msg.Params) != want {
			t.Errorf("event %d = %s %s, want %s", i, msg.Method, msg.Params, want)
		}
	}
	if last := msgs[2]; last.ID == nil || *last.ID != jsonrpc.NewIntID(9) || string(last.Result) != `{"done":true}` {
		t.Errorf("last event = %+v, want the result for id 9", last)
	}
}

func TestProgressTokenWithoutProgressIsPlainJSON(t *testing.T) {
	tracker := progress.NewTracker()
	tests := []struct {
		name   string
		answer *jsonrpc.Message
		status int
	}{
		{name: "result", answer: &jsonrpc.Message{Result: json.RawMessage(`{}`)}, status: http.StatusOK},
		{name: "method not found keeps its 404", answer: &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "nope"}}, status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, progressHandler(tracker, 0, tt.answer), Options{Progress: tracker})
			resp := post(t, srv, "tools/list", withProgress("1", `"p"`), nil)
			wantStatus(t, resp, tt.status)
			decode(t, resp) // checks Content-Type is application/json
		})
	}
}

func TestWithoutTrackerProgressTokenPassesThrough(t *testing.T) {
	seen := make(chan string, 1)
	h := &fakeHandler{answer: func(_ context.Context, _ string, params json.RawMessage) (*jsonrpc.Message, error) {
		seen <- string(params)
		return &jsonrpc.Message{Result: json.RawMessage(`{}`)}, nil
	}}
	srv := newServer(t, h, Options{})
	wantStatus(t, post(t, srv, "tools/list", withProgress("1", `"p"`), nil), http.StatusOK)
	if params := <-seen; !strings.Contains(params, `"progressToken":"p"`) {
		t.Fatalf("handler saw %s, want the client's token unchanged", params)
	}
}
