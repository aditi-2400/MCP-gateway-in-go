package policy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// fakeRouter records what reaches it and answers with canned replies.
type fakeRouter struct {
	mu        sync.Mutex
	calls     []string // method, or "tools/call <name>"
	listReply *jsonrpc.Message
	listErr   error
	schemaFor []string
}

func (f *fakeRouter) Call(_ context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "tools/list":
		f.calls = append(f.calls, method)
		return f.listReply, f.listErr
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(params, &p)
		f.calls = append(f.calls, method+" "+p.Name)
		return &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete","content":[]}`)}, nil
	default:
		f.calls = append(f.calls, method)
		return &jsonrpc.Message{Result: json.RawMessage(`{}`)}, nil
	}
}

func (f *fakeRouter) Notify(context.Context, string, json.RawMessage) error { return nil }

func (f *fakeRouter) ToolSchema(_ context.Context, tool string, _ json.RawMessage) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schemaFor = append(f.schemaFor, tool)
	return json.RawMessage(`{"type":"object"}`), true, nil
}

func (f *fakeRouter) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

const fullList = `{"resultType":"complete","tools":[` +
	`{"name":"fs__read_file","description":"read"},` +
	`{"name":"fs__write_file"},` +
	`{"name":"fs__delete"},` +
	`{"name":"db__query"},` +
	`{"name":"github__search"},` +
	`{"nameless":true},` +
	`"not a tool"` +
	`],"ttlMs":5000}`

func newTestGuard(t *testing.T) (*Guard, *fakeRouter) {
	t.Helper()
	router := &fakeRouter{listReply: &jsonrpc.Message{Result: json.RawMessage(fullList)}}
	return NewGuard(router, router, mustParse(t, argsPolicy), nil), router
}

func asAgent(name string) context.Context {
	return auth.WithAgent(context.Background(), auth.Agent{Name: name})
}

func toolNames(t *testing.T, result json.RawMessage) []string {
	t.Helper()
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	names := make([]string, len(r.Tools))
	for i, tool := range r.Tools {
		names[i] = tool.Name
	}
	return names
}

func TestGuardFiltersToolsList(t *testing.T) {
	g, _ := newTestGuard(t)
	tests := []struct {
		agent string
		want  string
	}{
		{"bot", "fs__read_file,fs__write_file,db__query"},
		{"pinger", ""},
		{"stranger", ""},
	}
	for _, tt := range tests {
		resp, err := g.Call(asAgent(tt.agent), "tools/list", json.RawMessage(`{}`))
		if err != nil || resp.Error != nil {
			t.Fatalf("%s: tools/list = %v, %v", tt.agent, resp.Error, err)
		}
		if got := strings.Join(toolNames(t, resp.Result), ","); got != tt.want {
			t.Errorf("%s sees %q, want %q", tt.agent, got, tt.want)
		}
		// Other result fields and each tool's own fields pass through.
		if !strings.Contains(string(resp.Result), `"ttlMs":5000`) {
			t.Errorf("%s: result lost ttlMs: %s", tt.agent, resp.Result)
		}
	}
	resp, _ := g.Call(asAgent("bot"), "tools/list", json.RawMessage(`{}`))
	if !strings.Contains(string(resp.Result), `"description":"read"`) {
		t.Errorf("tool fields changed: %s", resp.Result)
	}
}

func TestGuardListFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		reply *jsonrpc.Message
	}{
		{"result not an object", &jsonrpc.Message{Result: json.RawMessage(`[1]`)}},
		{"no tools field", &jsonrpc.Message{Result: json.RawMessage(`{"resultType":"complete"}`)}},
		{"tools not a list", &jsonrpc.Message{Result: json.RawMessage(`{"tools":{"fs__delete":{}}}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, router := newTestGuard(t)
			router.listReply = tt.reply
			resp, err := g.Call(asAgent("bot"), "tools/list", json.RawMessage(`{}`))
			if err != nil || resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError {
				t.Fatalf("tools/list = %+v, %v; want a -32603 error", resp, err)
			}
			if resp.Result != nil {
				t.Errorf("unfiltered result leaked: %s", resp.Result)
			}
		})
	}
}

func TestGuardPassesListErrorsThrough(t *testing.T) {
	g, router := newTestGuard(t)
	router.listReply = &jsonrpc.Message{Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "no upstream answered tools/list"}}
	resp, err := g.Call(asAgent("bot"), "tools/list", json.RawMessage(`{}`))
	if err != nil || resp.Error == nil || resp.Error.Message != "no upstream answered tools/list" {
		t.Fatalf("tools/list = %+v, %v", resp, err)
	}

	router.listReply, router.listErr = nil, context.DeadlineExceeded
	if _, err := g.Call(asAgent("bot"), "tools/list", json.RawMessage(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the router's error", err)
	}
}

func TestGuardToolsCall(t *testing.T) {
	tests := []struct {
		name      string
		agent     string
		params    string
		forwarded bool
		wantCode  int    // JSON-RPC error code, 0 for none
		wantText  string // text of an isError result
	}{
		{name: "allowed, no arg rule", agent: "bot", params: `{"name":"fs__read_file","arguments":{"path":"/etc/hosts"}}`, forwarded: true},
		{name: "allowed with args", agent: "bot", params: `{"name":"fs__write_file","arguments":{"path":"/tmp/agent-workspace/a","content":"hi"}}`, forwarded: true},
		{name: "tool not allowed looks unknown", agent: "bot", params: `{"name":"fs__delete","arguments":{}}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "agent not in policy", agent: "stranger", params: `{"name":"fs__read_file"}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "bad arguments are a tool error", agent: "bot", params: `{"name":"fs__write_file","arguments":{"path":"/etc/passwd","content":"hi"}}`, wantText: `Denied by gateway policy: argument "path" is outside the allowed folders`},
		{name: "pattern hidden from agent", agent: "bot", params: `{"name":"db__query","arguments":{"sql":"DROP TABLE t"}}`, wantText: `Denied by gateway policy: argument "sql" matches a denied pattern`},
		{name: "params not an object", agent: "bot", params: `[1]`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "no tool name", agent: "bot", params: `{"arguments":{}}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "tool name not a string", agent: "bot", params: `{"name":7}`, wantCode: jsonrpc.CodeInvalidParams},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, router := newTestGuard(t)
			resp, err := g.Call(asAgent(tt.agent), "tools/call", json.RawMessage(tt.params))
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			if got := len(router.seen()) > 0; got != tt.forwarded {
				t.Fatalf("forwarded = %v, want %v (router saw %v)", got, tt.forwarded, router.seen())
			}
			switch {
			case tt.wantCode != 0:
				if resp.Error == nil || resp.Error.Code != tt.wantCode {
					t.Fatalf("response = %+v, want error code %d", resp, tt.wantCode)
				}
			case tt.wantText != "":
				var r struct {
					ResultType string `json:"resultType"`
					IsError    bool   `json:"isError"`
					Content    []struct {
						Type, Text string
					} `json:"content"`
				}
				if resp.Error != nil || json.Unmarshal(resp.Result, &r) != nil {
					t.Fatalf("response = %+v, want an isError result", resp)
				}
				if !r.IsError || r.ResultType != "complete" || len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text != tt.wantText {
					t.Fatalf("result = %s, want isError text %q", resp.Result, tt.wantText)
				}
			default:
				if resp.Error != nil {
					t.Fatalf("response error = %v, want the upstream result", resp.Error)
				}
			}
		})
	}
}

// A hidden tool and a missing tool must answer the same way.
func TestGuardHiddenToolLooksMissing(t *testing.T) {
	g, _ := newTestGuard(t)
	hidden, _ := g.Call(asAgent("bot"), "tools/call", json.RawMessage(`{"name":"fs__delete"}`))
	missing, _ := g.Call(asAgent("bot"), "tools/call", json.RawMessage(`{"name":"fs__nope"}`))
	if hidden.Error == nil || missing.Error == nil {
		t.Fatalf("hidden = %+v, missing = %+v; want errors", hidden, missing)
	}
	if hidden.Error.Code != missing.Error.Code || hidden.Error.Message != "Unknown tool: fs__delete" || missing.Error.Message != "Unknown tool: fs__nope" {
		t.Errorf("hidden = %v, missing = %v; want the same shape", hidden.Error, missing.Error)
	}
}

func TestGuardMethods(t *testing.T) {
	g, router := newTestGuard(t)
	if resp, err := g.Call(asAgent("stranger"), "server/discover", json.RawMessage(`{}`)); err != nil || resp.Error != nil {
		t.Fatalf("server/discover = %+v, %v; want it passed through for any agent", resp, err)
	}
	for _, method := range []string{"resources/read", "prompts/get", "completion/complete", ""} {
		resp, err := g.Call(asAgent("bot"), method, json.RawMessage(`{}`))
		if err != nil || resp.Error == nil || resp.Error.Code != jsonrpc.CodeMethodNotFound {
			t.Errorf("%q = %+v, %v; want method not found", method, resp, err)
		}
	}
	if got := strings.Join(router.seen(), ","); got != "server/discover" {
		t.Errorf("router saw %q, want only server/discover", got)
	}
}

func TestGuardWithoutAgentFailsClosed(t *testing.T) {
	g, router := newTestGuard(t)
	for _, method := range []string{"server/discover", "tools/list", "tools/call"} {
		resp, err := g.Call(context.Background(), method, json.RawMessage(`{"name":"fs__read_file"}`))
		if err != nil || resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError {
			t.Errorf("%s = %+v, %v; want -32603", method, resp, err)
		}
	}
	if seen := router.seen(); len(seen) != 0 {
		t.Errorf("router saw %v, want nothing", seen)
	}
}

func TestGuardToolSchema(t *testing.T) {
	g, router := newTestGuard(t)
	if _, found, err := g.ToolSchema(asAgent("bot"), "fs__write_file", nil); !found || err != nil {
		t.Fatalf("allowed tool: found = %v, err = %v", found, err)
	}
	if _, found, err := g.ToolSchema(asAgent("bot"), "fs__delete", nil); found || err != nil {
		t.Fatalf("hidden tool: found = %v, err = %v; want not found", found, err)
	}
	if _, found, _ := g.ToolSchema(context.Background(), "fs__write_file", nil); found {
		t.Fatal("no agent: found, want not found")
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if strings.Join(router.schemaFor, ",") != "fs__write_file" {
		t.Errorf("schema lookups reached the router for %v, want only fs__write_file", router.schemaFor)
	}

	noSchemas := NewGuard(router, nil, g.policy, nil)
	if _, _, err := noSchemas.ToolSchema(asAgent("bot"), "fs__write_file", nil); err == nil {
		t.Error("guard without a schema source: error = nil, want one")
	}
}
