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
	run          func(args json.RawMessage) callResult
}

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
func callTool(params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
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

	for _, t := range tools {
		if t.Name == p.Name {
			return t.run(p.Arguments), nil
		}
	}
	return nil, invalidParams("Unknown tool: " + p.Name)
}

func runEcho(args json.RawMessage) callResult {
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

func runAdd(args json.RawMessage) callResult {
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

func runFail(json.RawMessage) callResult {
	return toolError("this tool always fails")
}
