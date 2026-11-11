package fakeupstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const validMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// request builds one request line with valid _meta plus any extra params.
func request(id, method, extraParams string) string {
	params := validMeta
	if extraParams != "" {
		params += "," + extraParams
	}
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":{` + params + `}}`
}

// serve runs the server over the given input lines and returns every response.
func serve(t *testing.T, lines ...string) []*jsonrpc.Message {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	if err := Serve(in, &out, jsonrpc.DefaultMaxMessageSize); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	r := jsonrpc.NewReader(&out, jsonrpc.DefaultMaxMessageSize)
	var responses []*jsonrpc.Message
	for {
		msg, err := r.Read()
		if errors.Is(err, io.EOF) {
			return responses
		}
		if err != nil {
			t.Fatalf("server wrote an invalid message: %v", err)
		}
		if msg.Kind() != jsonrpc.KindResponse {
			t.Fatalf("server wrote a %v, want only responses", msg.Kind())
		}
		responses = append(responses, msg)
	}
}

// serveOne sends one line and expects exactly one response.
func serveOne(t *testing.T, line string) *jsonrpc.Message {
	t.Helper()
	responses := serve(t, line)
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1", len(responses))
	}
	return responses[0]
}

func decodeResult(t *testing.T, msg *jsonrpc.Message, into any) {
	t.Helper()
	if msg.Error != nil {
		t.Fatalf("got error response %v, want result", msg.Error)
	}
	if err := json.Unmarshal(msg.Result, into); err != nil {
		t.Fatalf("decode result %s: %v", msg.Result, err)
	}
}

func TestDiscover(t *testing.T) {
	resp := serveOne(t, request(`1`, "server/discover", ""))

	var result struct {
		ResultType        string                     `json:"resultType"`
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		Meta              map[string]json.RawMessage `json:"_meta"`
	}
	decodeResult(t, resp, &result)

	if result.ResultType != "complete" {
		t.Errorf("resultType = %q, want complete", result.ResultType)
	}
	if len(result.SupportedVersions) != 1 || result.SupportedVersions[0] != ProtocolVersion {
		t.Errorf("supportedVersions = %v, want [%s]", result.SupportedVersions, ProtocolVersion)
	}
	if _, ok := result.Capabilities["tools"]; !ok {
		t.Error("capabilities is missing tools")
	}
	if _, ok := result.Meta["io.modelcontextprotocol/serverInfo"]; !ok {
		t.Error("_meta is missing serverInfo")
	}
}

func TestToolsList(t *testing.T) {
	resp := serveOne(t, request(`1`, "tools/list", ""))

	var result struct {
		ResultType string `json:"resultType"`
		Tools      []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	decodeResult(t, resp, &result)

	if result.ResultType != "complete" {
		t.Errorf("resultType = %q, want complete", result.ResultType)
	}
	var names []string
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
		var schema struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema.Type != "object" {
			t.Errorf("tool %q inputSchema = %s, want an object schema", tool.Name, tool.InputSchema)
		}
	}
	// Order must be stable so clients can cache the list.
	if got := strings.Join(names, ","); got != "echo,add,fail,count,lookup" {
		t.Errorf("tool names = %s, want echo,add,fail,count,lookup", got)
	}
}

// decodedCallResult is how a client sees a tools/call result on the wire.
type decodedCallResult struct {
	ResultType string `json:"resultType"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func TestToolsCall(t *testing.T) {
	tests := []struct {
		name           string
		params         string
		wantText       string
		wantIsError    bool
		wantStructured string
	}{
		{
			name:     "echo returns its text",
			params:   `"name":"echo","arguments":{"text":"hello"}`,
			wantText: "hello",
		},
		{
			name:        "echo without text is a tool error",
			params:      `"name":"echo","arguments":{}`,
			wantText:    "missing required argument: text",
			wantIsError: true,
		},
		{
			name:        "echo with wrong argument type is a tool error",
			params:      `"name":"echo","arguments":{"text":5}`,
			wantIsError: true,
		},
		{
			name:           "add returns text and structured content",
			params:         `"name":"add","arguments":{"a":2,"b":3.5}`,
			wantText:       "5.5",
			wantStructured: `{"sum":5.5}`,
		},
		{
			name:        "add without b is a tool error",
			params:      `"name":"add","arguments":{"a":2}`,
			wantText:    "missing required argument: b",
			wantIsError: true,
		},
		{
			name:        "fail always reports a tool error",
			params:      `"name":"fail","arguments":{}`,
			wantText:    "this tool always fails",
			wantIsError: true,
		},
		{
			name:        "missing arguments are treated as empty",
			params:      `"name":"echo"`,
			wantText:    "missing required argument: text",
			wantIsError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := serveOne(t, request(`1`, "tools/call", tt.params))
			var result decodedCallResult
			decodeResult(t, resp, &result)

			if result.ResultType != "complete" {
				t.Errorf("resultType = %q, want complete", result.ResultType)
			}
			if result.IsError != tt.wantIsError {
				t.Errorf("isError = %v, want %v", result.IsError, tt.wantIsError)
			}
			if len(result.Content) != 1 || result.Content[0].Type != "text" {
				t.Fatalf("content = %+v, want one text item", result.Content)
			}
			if tt.wantText != "" && result.Content[0].Text != tt.wantText {
				t.Errorf("text = %q, want %q", result.Content[0].Text, tt.wantText)
			}
			if tt.wantStructured != "" && string(result.StructuredContent) != tt.wantStructured {
				t.Errorf("structuredContent = %s, want %s", result.StructuredContent, tt.wantStructured)
			}
		})
	}
}

func TestProtocolErrors(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantCode int
		wantMsg  string
	}{
		{
			name:     "unknown method",
			line:     request(`1`, "resources/list", ""),
			wantCode: jsonrpc.CodeMethodNotFound,
		},
		{
			name:     "legacy initialize names the supported version",
			line:     `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
			wantCode: jsonrpc.CodeMethodNotFound,
			wantMsg:  ProtocolVersion,
		},
		{
			name:     "unknown tool",
			line:     request(`1`, "tools/call", `"name":"nope","arguments":{}`),
			wantCode: jsonrpc.CodeInvalidParams,
			wantMsg:  "Unknown tool: nope",
		},
		{
			name:     "missing tool name",
			line:     request(`1`, "tools/call", `"arguments":{}`),
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "arguments not an object",
			line:     request(`1`, "tools/call", `"name":"echo","arguments":"hello"`),
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "no params at all",
			line:     `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "missing _meta",
			line:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "missing protocol version",
			line:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "missing client capabilities",
			line:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "client capabilities not an object",
			line:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":[]}}}`,
			wantCode: jsonrpc.CodeInvalidParams,
		},
		{
			name:     "protocol version is not a string",
			line:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":20260728,"io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantCode: jsonrpc.CodeInvalidParams,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := serveOne(t, tt.line)
			if resp.Error == nil {
				t.Fatalf("got result %s, want error %d", resp.Result, tt.wantCode)
			}
			if resp.Error.Code != tt.wantCode {
				t.Errorf("code = %d, want %d (message: %s)", resp.Error.Code, tt.wantCode, resp.Error.Message)
			}
			if tt.wantMsg != "" && !strings.Contains(resp.Error.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", resp.Error.Message, tt.wantMsg)
			}
			if resp.ID == nil || *resp.ID != jsonrpc.NewIntID(1) {
				t.Errorf("id = %v, want 1", resp.ID)
			}
		})
	}
}

func TestUnsupportedProtocolVersion(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`
	resp := serveOne(t, line)

	if resp.Error == nil || resp.Error.Code != CodeUnsupportedProtocolVersion {
		t.Fatalf("error = %v, want code %d", resp.Error, CodeUnsupportedProtocolVersion)
	}
	var data struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}
	if err := json.Unmarshal(resp.Error.Data, &data); err != nil {
		t.Fatalf("decode error data %s: %v", resp.Error.Data, err)
	}
	if len(data.Supported) != 1 || data.Supported[0] != ProtocolVersion || data.Requested != "1900-01-01" {
		t.Errorf("error data = %+v, want supported [%s] and requested 1900-01-01", data, ProtocolVersion)
	}
}

func TestResponsesKeepRequestIDs(t *testing.T) {
	responses := serve(t,
		request(`"abc"`, "tools/list", ""),
		request(`7`, "server/discover", ""),
	)
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(responses))
	}
	if *responses[0].ID != jsonrpc.NewStringID("abc") {
		t.Errorf("first id = %v, want \"abc\"", responses[0].ID)
	}
	if *responses[1].ID != jsonrpc.NewIntID(7) {
		t.Errorf("second id = %v, want 7", responses[1].ID)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	responses := serve(t,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		request(`2`, "tools/list", ""),
	)
	if len(responses) != 1 || *responses[0].ID != jsonrpc.NewIntID(2) {
		t.Fatalf("responses = %v, want only the response to id 2", responses)
	}
}

func TestBadLineGetsErrorAndServerKeepsGoing(t *testing.T) {
	responses := serve(t,
		`this is not json`,
		request(`2`, "tools/list", ""),
	)
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(responses))
	}
	first := responses[0]
	if first.Error == nil || first.Error.Code != jsonrpc.CodeParseError || first.ID != nil {
		t.Errorf("first response = %+v, want parse error with null id", first)
	}
	if responses[1].Error != nil {
		t.Errorf("second response error = %v, want result", responses[1].Error)
	}
}

func TestOversizedLineGetsErrorAndServerKeepsGoing(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader(strings.Repeat("x", 100) + "\n" + request(`2`, "tools/list", "") + "\n")
	if err := Serve(in, &out, 64); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	// The valid request is also over 64 bytes, so both lines are rejected.
	// What matters is that the server answered both and did not stop.
	if got := strings.Count(out.String(), "\n"); got != 2 {
		t.Fatalf("got %d responses, want 2:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), `"code":-32600`) {
		t.Errorf("output = %s, want invalid request errors", out.String())
	}
}

func TestServeReturnsReadErrors(t *testing.T) {
	boom := errors.New("stdin broke")
	err := Serve(iotest.ErrReader(boom), io.Discard, jsonrpc.DefaultMaxMessageSize)
	if !errors.Is(err, boom) {
		t.Fatalf("Serve() error = %v, want %v", err, boom)
	}
}

// serveAll runs the server and returns every message it wrote, in order.
func serveAll(t *testing.T, lines ...string) []*jsonrpc.Message {
	t.Helper()
	var out bytes.Buffer
	if err := Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, jsonrpc.DefaultMaxMessageSize); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	r := jsonrpc.NewReader(&out, jsonrpc.DefaultMaxMessageSize)
	var msgs []*jsonrpc.Message
	for {
		msg, err := r.Read()
		if errors.Is(err, io.EOF) {
			return msgs
		}
		if err != nil {
			t.Fatalf("server wrote an invalid message: %v", err)
		}
		msgs = append(msgs, msg)
	}
}

func TestCountSendsProgressBeforeTheResult(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"count","arguments":{"n":3},` +
		`"_meta":{"progressToken":"tok-7","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	msgs := serveAll(t, line)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 3 progress notifications and 1 response", len(msgs))
	}
	for i, msg := range msgs[:3] {
		want := `{"progressToken":"tok-7","progress":` + strconv.Itoa(i+1) + `,"total":3}`
		if msg.Method != "notifications/progress" || string(msg.Params) != want {
			t.Errorf("message %d = %s %s, want progress %s", i, msg.Method, msg.Params, want)
		}
	}
	if msgs[3].Kind() != jsonrpc.KindResponse || !strings.Contains(string(msgs[3].Result), "counted to 3") {
		t.Errorf("last message = %+v, want the result", msgs[3])
	}
}

func TestCountWithoutProgressTokenSendsOnlyTheResult(t *testing.T) {
	msgs := serveAll(t, request(`1`, "tools/call", `"name":"count","arguments":{"n":3}`))
	if len(msgs) != 1 || msgs[0].Kind() != jsonrpc.KindResponse {
		t.Fatalf("got %d messages, want only the response", len(msgs))
	}
}

func TestCountRejectsBadN(t *testing.T) {
	for _, args := range []string{`{}`, `{"n":0}`, `{"n":101}`, `{"n":"3"}`} {
		var result decodedCallResult
		decodeResult(t, serveOne(t, request(`1`, "tools/call", `"name":"count","arguments":`+args)), &result)
		if !result.IsError {
			t.Errorf("count %s: isError = false, want true", args)
		}
	}
}

func TestLookupEchoesArguments(t *testing.T) {
	var result struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	decodeResult(t, serveOne(t, request(`1`, "tools/call", `"name":"lookup","arguments":{"region":"us-west1","limit":5}`)), &result)
	if string(result.StructuredContent) != `{"region":"us-west1","limit":5}` {
		t.Fatalf("structuredContent = %s, want the arguments", result.StructuredContent)
	}
}
