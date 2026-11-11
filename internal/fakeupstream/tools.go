package fakeupstream

import (
	"encoding/json"
	"strconv"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// tool is one fake tool: what tools/list shows, plus the function that runs it.
type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	run          func(args json.RawMessage, report progressFunc) callResult
}

// progressFunc sends a progress notification for the current call. It does
// nothing when the client did not ask for progress.
type progressFunc func(progress, total float64)

// tools is a slice, not a map, so tools/list always returns the same order.
var tools = []tool{
	{
		Name:        "echo",
		Description: "Returns the given text unchanged.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		run:         runEcho,
	},
	{
		Name:         "add",
		Description:  "Adds two numbers.",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"sum":{"type":"number"}},"required":["sum"]}`),
		run:          runAdd,
	},
	{
		Name:        "fail",
		Description: "Always returns a tool execution error. Used to test error passthrough.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		run:         runFail,
	},
	{
		Name:        "count",
		Description: "Counts to n, sending a progress notification for each step when the call has a progressToken.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer","minimum":1,"maximum":100}},"required":["n"]}`),
		run:         runCount,
	},
	{
		Name:        "lookup",
		Description: "Echoes its arguments. Its schema mirrors region, limit, and verbose into Mcp-Param-* HTTP headers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"region":{"type":"string","x-mcp-header":"Region"},` +
			`"limit":{"type":"integer","x-mcp-header":"Limit"},` +
			`"verbose":{"type":"boolean","x-mcp-header":"Verbose"},` +
			`"filter":{"type":"object","properties":{"owner":{"type":"string","x-mcp-header":"Owner"}}},` +
			`"query":{"type":"string"}},"required":["region"]}`),
		run: runLookup,
	},
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	ResultType        string        `json:"resultType"`
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

func textResult(text string) callResult {
	return callResult{ResultType: "complete", Content: []textContent{{Type: "text", Text: text}}}
}

// toolError is a tool execution error: a normal result with isError set, so
// the model can read the message and try again.
func toolError(text string) callResult {
	r := textResult(text)
	r.IsError = true
	return r
}

func toolsListResult() any {
	return struct {
		ResultType string `json:"resultType"`
		Tools      []tool `json:"tools"`
	}{"complete", tools}
}

// callTool runs a tools/call request. Problems with the request itself
// (unknown tool, malformed params) are protocol errors; problems with the
// argument values are tool execution errors inside the result.
func callTool(params json.RawMessage, notify func(*jsonrpc.Message)) (any, *jsonrpc.Error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParams("invalid tools/call params: " + err.Error())
	}
	if p.Name == "" {
		return nil, invalidParams("missing tool name")
	}
	if p.Arguments == nil {
		p.Arguments = json.RawMessage(`{}`)
	}
	if p.Arguments[0] != '{' {
		return nil, invalidParams("arguments must be an object")
	}

	report := func(progress, total float64) {
		if p.Meta.ProgressToken == nil {
			return
		}
		data, err := json.Marshal(struct {
			ProgressToken json.RawMessage `json:"progressToken"`
			Progress      float64         `json:"progress"`
			Total         float64         `json:"total"`
		}{p.Meta.ProgressToken, progress, total})
		if err == nil {
			notify(jsonrpc.NewNotification("notifications/progress", data))
		}
	}
	for _, t := range tools {
		if t.Name == p.Name {
			return t.run(p.Arguments, report), nil
		}
	}
	return nil, invalidParams("Unknown tool: " + p.Name)
}

func runEcho(args json.RawMessage, _ progressFunc) callResult {
	var a struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolError("invalid arguments: " + err.Error())
	}
	if a.Text == nil {
		return toolError("missing required argument: text")
	}
	return textResult(*a.Text)
}

func runAdd(args json.RawMessage, _ progressFunc) callResult {
	var a struct {
		A *float64 `json:"a"`
		B *float64 `json:"b"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolError("invalid arguments: " + err.Error())
	}
	if a.A == nil {
		return toolError("missing required argument: a")
	}
	if a.B == nil {
		return toolError("missing required argument: b")
	}
	sum := *a.A + *a.B
	r := textResult(strconv.FormatFloat(sum, 'g', -1, 64))
	r.StructuredContent = map[string]float64{"sum": sum}
	return r
}

func runFail(json.RawMessage, progressFunc) callResult {
	return toolError("this tool always fails")
}

func runCount(args json.RawMessage, report progressFunc) callResult {
	var a struct {
		N *int `json:"n"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.N == nil || *a.N < 1 || *a.N > 100 {
		return toolError("n must be an integer from 1 to 100")
	}
	for i := 1; i <= *a.N; i++ {
		report(float64(i), float64(*a.N))
	}
	return textResult("counted to " + strconv.Itoa(*a.N))
}

func runLookup(args json.RawMessage, _ progressFunc) callResult {
	r := textResult(string(args))
	r.StructuredContent = args
	return r
}
