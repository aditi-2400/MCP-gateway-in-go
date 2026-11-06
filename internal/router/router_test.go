package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

const testTimeout = 5 * time.Second

const validMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

var clientListParams = json.RawMessage(`{"_meta":` + validMeta + `}`)

// fakeUpstream records every call and answers with a function the test picks.
type fakeUpstream struct {
	mu     sync.Mutex
	calls  []string // params of each call
	answer func(params json.RawMessage) (*jsonrpc.Message, error)
}

func (f *fakeUpstream) Call(_ context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+string(params))
	f.mu.Unlock()
	return f.answer(params)
}

func (f *fakeUpstream) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func result(body string) *jsonrpc.Message {
	return &jsonrpc.Message{Result: json.RawMessage(body)}
}

// listing answers tools/list with a single page holding these tools.
func listing(tools ...string) *fakeUpstream {
	return &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) {
		return result(`{"resultType":"complete","tools":[` + strings.Join(tools, ",") + `]}`), nil
	}}
}

func failing(err error) *fakeUpstream {
	return &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) { return nil, err }}
}

func newRouter(t *testing.T, upstreams ...Named) *Router {
	t.Helper()
	r, err := New(upstreams, Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r
}

// toolNames calls tools/list and returns the names in order.
func toolNames(t *testing.T, r *Router) []string {
	t.Helper()
	resp := call(t, r, "tools/list", clientListParams)
	if resp.Error != nil {
		t.Fatalf("tools/list error = %v", resp.Error)
	}
	var body struct {
		ResultType string `json:"resultType"`
		Tools      []struct {
			Name string `json:"name"`
		} `json:"tools"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode tools/list result %s: %v", resp.Result, err)
	}
	if body.ResultType != "complete" || body.NextCursor != nil {
		t.Fatalf("result = %s, want resultType complete and no nextCursor", resp.Result)
	}
	names := []string{}
	for _, tool := range body.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func call(t *testing.T, r *Router, method string, params json.RawMessage) *jsonrpc.Message {
	t.Helper()
	resp, err := r.Call(context.Background(), method, params)
	if err != nil {
		t.Fatalf("Call(%s) error = %v", method, err)
	}
	if resp == nil {
		t.Fatalf("Call(%s) = nil response", method)
	}
	return resp
}

func wantCode(t *testing.T, resp *jsonrpc.Message, code int) {
	t.Helper()
	if resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("response = result %s, error %v, want error code %d", resp.Result, resp.Error, code)
	}
}

func TestNewRejectsBadNames(t *testing.T) {
	up := listing()
	tests := []struct {
		name      string
		upstreams []Named
	}{
		{name: "no upstreams", upstreams: nil},
		{name: "empty name", upstreams: []Named{{Name: "", Upstream: up}}},
		{name: "underscore", upstreams: []Named{{Name: "a_", Upstream: up}}},
		{name: "duplicate", upstreams: []Named{{Name: "fs", Upstream: up}, {Name: "fs", Upstream: up}}},
		{name: "nil upstream", upstreams: []Named{{Name: "fs"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.upstreams, Options{}); err == nil {
				t.Fatal("New() error = nil, want an error")
			}
		})
	}
}

func TestDiscoverIsAnsweredByTheGateway(t *testing.T) {
	up := listing()
	r := newRouter(t, Named{Name: "fs", Upstream: up})

	resp := call(t, r, "server/discover", clientListParams)
	if resp.Error != nil {
		t.Fatalf("discover error = %v", resp.Error)
	}
	for _, want := range []string{`"resultType":"complete"`, `"supportedVersions":["2026-07-28"]`, `"capabilities":{"tools":{}}`, `"name":"mcp-gateway"`} {
		if !strings.Contains(string(resp.Result), want) {
			t.Errorf("discover result = %s, want it to contain %s", resp.Result, want)
		}
	}
	if calls := up.callLog(); len(calls) != 0 {
		t.Errorf("upstream got calls %v, want none", calls)
	}
}

func TestRequestsWithBadMetaNeverReachUpstreams(t *testing.T) {
	up := listing(`{"name":"echo"}`)
	r := newRouter(t, Named{Name: "fs", Upstream: up})
	oldVersion := json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}`)

	for _, method := range []string{"server/discover", "tools/list"} {
		wantCode(t, call(t, r, method, oldVersion), mcp.CodeUnsupportedProtocolVersion)
		wantCode(t, call(t, r, method, nil), jsonrpc.CodeInvalidParams)
	}
	if calls := up.callLog(); len(calls) != 0 {
		t.Errorf("upstream got calls %v, want none", calls)
	}
}

func TestUnsupportedMethods(t *testing.T) {
	up := listing()
	r := newRouter(t, Named{Name: "fs", Upstream: up})
	for _, method := range []string{"initialize", "resources/list", "prompts/get", "tools/calls", "nope"} {
		t.Run(method, func(t *testing.T) {
			wantCode(t, call(t, r, method, clientListParams), jsonrpc.CodeMethodNotFound)
		})
	}
	if calls := up.callLog(); len(calls) != 0 {
		t.Errorf("upstream got calls %v, want none", calls)
	}
}

func TestToolsListMergesAndPrefixesInConfigOrder(t *testing.T) {
	fs := listing(`{"name":"read_file"}`, `{"name":"search"}`)
	gh := listing(`{"name":"search"}`, `{"name":"create_issue"}`)
	r := newRouter(t, Named{Name: "fs", Upstream: fs}, Named{Name: "github", Upstream: gh})

	got := strings.Join(toolNames(t, r), " ")
	want := "fs__read_file fs__search github__search github__create_issue"
	if got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}
	// Order must not change between requests (the spec asks for this so
	// clients can cache).
	if again := strings.Join(toolNames(t, r), " "); again != want {
		t.Fatalf("second tools/list = %s, want %s", again, want)
	}
}

func TestToolsListKeepsOtherToolFieldsUnchanged(t *testing.T) {
	tool := `{"name":"add","title":"Add","description":"Adds","inputSchema":{"type":"object","properties":{"a":{"type":"number"}}},"annotations":{"readOnlyHint":true},"x-extra":[1,2]}`
	r := newRouter(t, Named{Name: "calc", Upstream: listing(tool)})

	resp := call(t, r, "tools/list", clientListParams)
	var body struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil || len(body.Tools) != 1 {
		t.Fatalf("result = %s, err %v", resp.Result, err)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(tool), &original); err != nil {
		t.Fatal(err)
	}
	got := body.Tools[0]
	if string(got["name"]) != `"calc__add"` {
		t.Errorf("name = %s, want \"calc__add\"", got["name"])
	}
	for key, value := range original {
		if key == "name" {
			continue
		}
		if string(got[key]) != string(value) {
			t.Errorf("field %s = %s, want %s", key, got[key], value)
		}
	}
	if len(got) != len(original) {
		t.Errorf("tool has %d fields, want %d", len(got), len(original))
	}
}

func TestToolsListForwardsClientMeta(t *testing.T) {
	up := listing(`{"name":"echo"}`)
	r := newRouter(t, Named{Name: "fs", Upstream: up})
	toolNames(t, r)

	calls := up.callLog()
	if len(calls) != 1 || calls[0] != `tools/list {"_meta":`+validMeta+`}` {
		t.Fatalf("upstream calls = %v, want one tools/list with the client's _meta and no cursor", calls)
	}
}

func TestToolsListFollowsPages(t *testing.T) {
	// Three pages. The second cursor is an empty string, which the spec says
	// is a real cursor, not the end.
	up := &fakeUpstream{}
	up.answer = func(params json.RawMessage) (*jsonrpc.Message, error) {
		var p struct {
			Cursor *string `json:"cursor"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		switch {
		case p.Cursor == nil:
			return result(`{"tools":[{"name":"a"}],"nextCursor":"page2"}`), nil
		case *p.Cursor == "page2":
			return result(`{"tools":[{"name":"b"}],"nextCursor":""}`), nil
		case *p.Cursor == "":
			return result(`{"tools":[{"name":"c"}],"nextCursor":null}`), nil
		}
		return nil, errors.New("unexpected cursor")
	}
	r := newRouter(t, Named{Name: "fs", Upstream: up})

	if got := strings.Join(toolNames(t, r), " "); got != "fs__a fs__b fs__c" {
		t.Fatalf("tools = %s, want fs__a fs__b fs__c", got)
	}
	if n := len(up.callLog()); n != 3 {
		t.Errorf("upstream calls = %d, want 3", n)
	}
}

func TestToolsListStopsAtPageLimit(t *testing.T) {
	endless := &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) {
		return result(`{"tools":[{"name":"again"}],"nextCursor":"same"}`), nil
	}}
	r, err := New([]Named{{Name: "loop", Upstream: endless}, {Name: "ok", Upstream: listing(`{"name":"x"}`)}}, Options{MaxPages: 5})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(toolNames(t, r), " "); got != "ok__x" {
		t.Fatalf("tools = %s, want only ok__x", got)
	}
	if n := len(endless.callLog()); n != 5 {
		t.Errorf("endless upstream got %d calls, want 5", n)
	}
}

func TestToolsListRejectsClientCursor(t *testing.T) {
	up := listing(`{"name":"echo"}`)
	r := newRouter(t, Named{Name: "fs", Upstream: up})

	// The gateway never hands out cursors, so any cursor is invalid.
	params := json.RawMessage(`{"cursor":"abc","_meta":` + validMeta + `}`)
	wantCode(t, call(t, r, "tools/list", params), jsonrpc.CodeInvalidParams)
	if calls := up.callLog(); len(calls) != 0 {
		t.Errorf("upstream got calls %v, want none", calls)
	}
}

func TestToolsListSkipsFailingUpstreams(t *testing.T) {
	tests := []struct {
		name string
		bad  *fakeUpstream
	}{
		{name: "connection closed", bad: failing(upstream.ErrClosed)},
		{name: "timed out", bad: failing(context.DeadlineExceeded)},
		{name: "error response", bad: &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) {
			return &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "boom"}}, nil
		}}},
		{name: "result is not an object", bad: &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) {
			return result(`[1,2]`), nil
		}}},
		{name: "tools is not a list", bad: &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) {
			return result(`{"tools":"echo"}`), nil
		}}},
		{name: "not complete", bad: &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) {
			return result(`{"resultType":"input_required","tools":[{"name":"x"}]}`), nil
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRouter(t,
				Named{Name: "bad", Upstream: tt.bad},
				Named{Name: "good", Upstream: listing(`{"name":"echo"}`)},
			)
			if got := strings.Join(toolNames(t, r), " "); got != "good__echo" {
				t.Fatalf("tools = %s, want good__echo", got)
			}
		})
	}
}

func TestToolsListErrorsWhenNoUpstreamAnswers(t *testing.T) {
	r := newRouter(t,
		Named{Name: "a", Upstream: failing(upstream.ErrClosed)},
		Named{Name: "b", Upstream: failing(context.DeadlineExceeded)},
	)
	resp := call(t, r, "tools/list", clientListParams)
	wantCode(t, resp, jsonrpc.CodeInternalError)
	// Upstream details stay in the log.
	if strings.Contains(resp.Error.Message, "closed") || strings.Contains(resp.Error.Message, "deadline") {
		t.Errorf("error message %q leaks upstream details", resp.Error.Message)
	}
}

func TestToolsListWithNoToolsIsAnEmptyList(t *testing.T) {
	r := newRouter(t, Named{Name: "fs", Upstream: listing()})
	resp := call(t, r, "tools/list", clientListParams)
	if !strings.Contains(string(resp.Result), `"tools":[]`) {
		t.Fatalf("result = %s, want an empty tools list (not null)", resp.Result)
	}
}

func TestToolsListSkipsMalformedTools(t *testing.T) {
	up := listing(
		`{"name":"good"}`,
		`"just a string"`,
		`null`,
		`{"description":"no name"}`,
		`{"name":""}`,
		`{"name":42}`,
		`{"name":"good"}`, // duplicate within one upstream: first wins
		`{"name":"also_good"}`,
	)
	r := newRouter(t, Named{Name: "fs", Upstream: up})

	if got := strings.Join(toolNames(t, r), " "); got != "fs__good fs__also_good" {
		t.Fatalf("tools = %s, want fs__good fs__also_good", got)
	}
}

func TestToolsListAsksUpstreamsConcurrently(t *testing.T) {
	// Each upstream waits until the other has been called. If the router
	// asked them one at a time, this would deadlock.
	var started sync.WaitGroup
	started.Add(2)
	both := func(json.RawMessage) (*jsonrpc.Message, error) {
		started.Done()
		started.Wait()
		return result(`{"tools":[{"name":"t"}]}`), nil
	}
	r := newRouter(t,
		Named{Name: "a", Upstream: &fakeUpstream{answer: both}},
		Named{Name: "b", Upstream: &fakeUpstream{answer: both}},
	)

	// t.Fatal must not be called from another goroutine, so the call runs
	// there and the checks run here.
	done := make(chan *jsonrpc.Message, 1)
	go func() {
		resp, _ := r.Call(context.Background(), "tools/list", clientListParams)
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp == nil || !strings.Contains(string(resp.Result), `"name":"a__t"},{"name":"b__t"`) {
			t.Fatalf("tools/list = %+v, want a__t then b__t", resp)
		}
	case <-time.After(testTimeout):
		t.Fatal("tools/list did not finish; upstreams were not asked concurrently")
	}
}

func callParams(fields string) json.RawMessage {
	return json.RawMessage(`{` + fields + `,"_meta":` + validMeta + `}`)
}

func TestToolsCallRoutesByPrefixAndStripsIt(t *testing.T) {
	answer := func(json.RawMessage) (*jsonrpc.Message, error) {
		return result(`{"resultType":"complete","content":[]}`), nil
	}
	fs := &fakeUpstream{answer: answer}
	gh := &fakeUpstream{answer: answer}
	r := newRouter(t, Named{Name: "fs", Upstream: fs}, Named{Name: "github", Upstream: gh})

	tests := []struct {
		name     string
		params   json.RawMessage
		wantUp   *fakeUpstream
		wantTool string
	}{
		{name: "simple", params: callParams(`"name":"github__search","arguments":{"q":"x"}`), wantUp: gh, wantTool: "search"},
		{name: "tool name with underscores", params: callParams(`"name":"fs__read_file","arguments":{}`), wantUp: fs, wantTool: "read_file"},
		{name: "tool name containing the separator", params: callParams(`"name":"fs__a__b"`), wantUp: fs, wantTool: "a__b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.wantUp.callLog())
			resp := call(t, r, "tools/call", tt.params)
			if resp.Error != nil {
				t.Fatalf("error = %v", resp.Error)
			}
			calls := tt.wantUp.callLog()
			if len(calls) != before+1 {
				t.Fatalf("upstream got %d new calls, want 1", len(calls)-before)
			}
			method, params, _ := strings.Cut(calls[len(calls)-1], " ")
			if method != "tools/call" {
				t.Fatalf("method = %s, want tools/call", method)
			}
			var sent map[string]json.RawMessage
			if err := json.Unmarshal([]byte(params), &sent); err != nil {
				t.Fatal(err)
			}
			if string(sent["name"]) != `"`+tt.wantTool+`"` {
				t.Errorf("upstream saw name %s, want %q", sent["name"], tt.wantTool)
			}
		})
	}
	if len(fs.callLog())+len(gh.callLog()) != len(tests) {
		t.Errorf("total upstream calls = %d, want %d (no cross routing)", len(fs.callLog())+len(gh.callLog()), len(tests))
	}
}

func TestToolsCallKeepsOtherParamsUnchanged(t *testing.T) {
	up := &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) { return result(`{}`), nil }}
	r := newRouter(t, Named{Name: "fs", Upstream: up})

	// inputResponses and requestState are how a client retries after an
	// input_required result; they must reach the upstream as sent.
	params := callParams(`"name":"fs__login","arguments":{"user":"a","n":[1,2]},"inputResponses":{"k":{"action":"accept"}},"requestState":"opaque=="`)
	call(t, r, "tools/call", params)

	var original, sent map[string]json.RawMessage
	if err := json.Unmarshal(params, &original); err != nil {
		t.Fatal(err)
	}
	_, sentParams, _ := strings.Cut(up.callLog()[0], " ")
	if err := json.Unmarshal([]byte(sentParams), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent) != len(original) {
		t.Fatalf("upstream params have %d fields, want %d: %s", len(sent), len(original), sentParams)
	}
	for key, value := range original {
		if key != "name" && string(sent[key]) != string(value) {
			t.Errorf("field %s = %s, want %s", key, sent[key], value)
		}
	}
}

func TestToolsCallPassesUpstreamAnswersThrough(t *testing.T) {
	upstreamErr := &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Unknown tool: nope", Data: json.RawMessage(`{"x":1}`)}
	tests := []struct {
		name   string
		answer *jsonrpc.Message
	}{
		{name: "result", answer: result(`{"resultType":"complete","content":[{"type":"text","text":"hi"}]}`)},
		{name: "tool error result", answer: result(`{"resultType":"complete","content":[],"isError":true}`)},
		{name: "input required", answer: result(`{"resultType":"input_required","inputRequests":{},"requestState":"s"}`)},
		{name: "protocol error", answer: &jsonrpc.Message{Error: upstreamErr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) { return tt.answer, nil }}
			r := newRouter(t, Named{Name: "fs", Upstream: up})

			resp := call(t, r, "tools/call", callParams(`"name":"fs__x"`))
			if string(resp.Result) != string(tt.answer.Result) || resp.Error != tt.answer.Error {
				t.Fatalf("response = result %s, error %v, want result %s, error %v", resp.Result, resp.Error, tt.answer.Result, tt.answer.Error)
			}
		})
	}
}

func TestToolsCallReturnsUpstreamFailureAsError(t *testing.T) {
	r := newRouter(t, Named{Name: "fs", Upstream: failing(upstream.ErrClosed)})
	resp, err := r.Call(context.Background(), "tools/call", callParams(`"name":"fs__x"`))
	if !errors.Is(err, upstream.ErrClosed) || resp != nil {
		t.Fatalf("Call() = %v, %v, want nil and ErrClosed", resp, err)
	}
}

func TestToolsCallBadNamesNeverReachUpstreams(t *testing.T) {
	up := &fakeUpstream{answer: func(json.RawMessage) (*jsonrpc.Message, error) { return result(`{}`), nil }}
	r := newRouter(t, Named{Name: "fs", Upstream: up})

	tests := []struct {
		name   string
		params json.RawMessage
	}{
		{name: "no prefix", params: callParams(`"name":"read_file"`)},
		{name: "unknown prefix", params: callParams(`"name":"db__query"`)},
		{name: "prefix differs in case", params: callParams(`"name":"FS__read_file"`)},
		{name: "empty tool part", params: callParams(`"name":"fs__"`)},
		{name: "empty prefix", params: callParams(`"name":"__read_file"`)},
		{name: "single underscore", params: callParams(`"name":"fs_read_file"`)},
		{name: "missing name", params: callParams(`"arguments":{}`)},
		{name: "empty name", params: callParams(`"name":""`)},
		{name: "name is a number", params: callParams(`"name":7`)},
		{name: "name is null", params: callParams(`"name":null`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantCode(t, call(t, r, "tools/call", tt.params), jsonrpc.CodeInvalidParams)
		})
	}

	oldVersion := json.RawMessage(`{"name":"fs__x","_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}`)
	wantCode(t, call(t, r, "tools/call", oldVersion), mcp.CodeUnsupportedProtocolVersion)
	wantCode(t, call(t, r, "tools/call", json.RawMessage(`{"name":"fs__x"}`)), jsonrpc.CodeInvalidParams)

	if calls := up.callLog(); len(calls) != 0 {
		t.Errorf("upstream got calls %v, want none", calls)
	}
}

func TestNotificationsFromClientAreDropped(t *testing.T) {
	up := listing()
	r := newRouter(t, Named{Name: "fs", Upstream: up})
	if err := r.Notify(context.Background(), "notifications/progress", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if calls := up.callLog(); len(calls) != 0 {
		t.Errorf("upstream got calls %v, want none", calls)
	}
}
