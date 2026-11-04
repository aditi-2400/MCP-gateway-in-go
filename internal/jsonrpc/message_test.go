package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantKind   Kind
		wantID     *ID
		wantMethod string
	}{
		{
			name:       "request with integer id",
			input:      `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantKind:   KindRequest,
			wantID:     ptr(NewIntID(1)),
			wantMethod: "tools/list",
		},
		{
			name:       "request with string id",
			input:      `{"jsonrpc":"2.0","id":"abc-123","method":"tools/call","params":{"name":"echo"}}`,
			wantKind:   KindRequest,
			wantID:     ptr(NewStringID("abc-123")),
			wantMethod: "tools/call",
		},
		{
			name:       "request with negative id",
			input:      `{"jsonrpc":"2.0","id":-7,"method":"ping"}`,
			wantKind:   KindRequest,
			wantID:     ptr(NewIntID(-7)),
			wantMethod: "ping",
		},
		{
			name:       "request with empty string id",
			input:      `{"jsonrpc":"2.0","id":"","method":"ping"}`,
			wantKind:   KindRequest,
			wantID:     ptr(NewStringID("")),
			wantMethod: "ping",
		},
		{
			name:       "notification has no id",
			input:      `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
			wantKind:   KindNotification,
			wantMethod: "notifications/cancelled",
		},
		{
			name:     "result response",
			input:    `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
			wantKind: KindResponse,
			wantID:   ptr(NewIntID(1)),
		},
		{
			name:     "error response",
			input:    `{"jsonrpc":"2.0","id":"x","error":{"code":-32601,"message":"Method not found"}}`,
			wantKind: KindResponse,
			wantID:   ptr(NewStringID("x")),
		},
		{
			name:     "error response with null id",
			input:    `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`,
			wantKind: KindResponse,
		},
		{
			name:     "error response with missing id",
			input:    `{"jsonrpc":"2.0","error":{"code":-32700,"message":"Parse error"}}`,
			wantKind: KindResponse,
		},
		{
			name:       "leading whitespace and unknown fields are fine",
			input:      " \t{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\",\"extra\":true}",
			wantKind:   KindRequest,
			wantID:     ptr(NewIntID(2)),
			wantMethod: "ping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse([]byte(tt.input))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if got := msg.Kind(); got != tt.wantKind {
				t.Errorf("Kind() = %v, want %v", got, tt.wantKind)
			}
			if !equalIDPtr(msg.ID, tt.wantID) {
				t.Errorf("ID = %v, want %v", msg.ID, tt.wantID)
			}
			if msg.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", msg.Method, tt.wantMethod)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantCode int
	}{
		{"empty input", ``, CodeParseError},
		{"not json", `hello`, CodeParseError},
		{"truncated json", `{"jsonrpc":"2.0","id":1`, CodeParseError},
		{"batch arrays are not supported", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, CodeInvalidRequest},
		{"top level string", `"ping"`, CodeInvalidRequest},
		{"missing jsonrpc version", `{"id":1,"method":"ping"}`, CodeInvalidRequest},
		{"wrong jsonrpc version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, CodeInvalidRequest},
		{"request with null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, CodeInvalidRequest},
		{"fractional id", `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, CodeInvalidRequest},
		{"exponent id", `{"jsonrpc":"2.0","id":1e3,"method":"ping"}`, CodeInvalidRequest},
		{"boolean id", `{"jsonrpc":"2.0","id":true,"method":"ping"}`, CodeInvalidRequest},
		{"object id", `{"jsonrpc":"2.0","id":{},"method":"ping"}`, CodeInvalidRequest},
		{"id too large for int64", `{"jsonrpc":"2.0","id":99999999999999999999,"method":"ping"}`, CodeInvalidRequest},
		{"empty method", `{"jsonrpc":"2.0","id":1,"method":""}`, CodeInvalidRequest},
		{"method is not a string", `{"jsonrpc":"2.0","id":1,"method":5}`, CodeInvalidRequest},
		{"params is an array", `{"jsonrpc":"2.0","id":1,"method":"ping","params":[1,2]}`, CodeInvalidRequest},
		{"params is a string", `{"jsonrpc":"2.0","id":1,"method":"ping","params":"x"}`, CodeInvalidRequest},
		{"params is null", `{"jsonrpc":"2.0","id":1,"method":"ping","params":null}`, CodeInvalidRequest},
		{"request with result", `{"jsonrpc":"2.0","id":1,"method":"ping","result":{}}`, CodeInvalidRequest},
		{"response with result and error", `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`, CodeInvalidRequest},
		{"response with neither result nor error", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{"result response without id", `{"jsonrpc":"2.0","result":{}}`, CodeInvalidRequest},
		{"result response with null id", `{"jsonrpc":"2.0","id":null,"result":{}}`, CodeInvalidRequest},
		{"error code is not an integer", `{"jsonrpc":"2.0","id":1,"error":{"code":1.5,"message":"x"}}`, CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse([]byte(tt.input))
			if err == nil {
				t.Fatalf("Parse() = %+v, want error", msg)
			}
			var rpcErr *Error
			if !errors.As(err, &rpcErr) {
				t.Fatalf("Parse() error type = %T, want *Error", err)
			}
			if rpcErr.Code != tt.wantCode {
				t.Errorf("error code = %d, want %d (message: %s)", rpcErr.Code, tt.wantCode, rpcErr.Message)
			}
		})
	}
}

func TestParseKeepsRawParamsAndResult(t *testing.T) {
	// The gateway forwards params and results untouched, so the exact bytes must survive.
	params := `{"name":"echo","arguments":{"text":"hi  there"}}`
	msg, err := Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + params + `}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if string(msg.Params) != params {
		t.Errorf("Params = %s, want %s", msg.Params, params)
	}

	result := `{"content":[{"type":"text","text":"hi"}]}`
	msg, err = Parse([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if string(msg.Result) != result {
		t.Errorf("Result = %s, want %s", msg.Result, result)
	}
}

func TestParseErrorResponseFields(t *testing.T) {
	msg, err := Parse([]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"bad","data":{"field":"x"}}}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if msg.Error == nil {
		t.Fatal("Error = nil, want error object")
	}
	if msg.Error.Code != CodeInvalidParams || msg.Error.Message != "bad" {
		t.Errorf("Error = %+v, want code %d and message %q", msg.Error, CodeInvalidParams, "bad")
	}
	if string(msg.Error.Data) != `{"field":"x"}` {
		t.Errorf("Error.Data = %s, want {\"field\":\"x\"}", msg.Error.Data)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
	}{
		{"request", NewRequest(NewIntID(1), "tools/list", json.RawMessage(`{"cursor":"a"}`))},
		{"request without params", NewRequest(NewStringID("r1"), "ping", nil)},
		{"notification", NewNotification("notifications/progress", json.RawMessage(`{"progress":1}`))},
		{"result response", NewResult(NewIntID(9), json.RawMessage(`{"tools":[]}`))},
		{"error response", NewErrorResponse(ptr(NewStringID("e")), &Error{Code: CodeMethodNotFound, Message: "nope"})},
		{"error response without id", NewErrorResponse(nil, &Error{Code: CodeParseError, Message: "bad json"})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			got, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse(%s) error = %v", data, err)
			}
			if got.Kind() != tt.msg.Kind() {
				t.Errorf("Kind() = %v, want %v", got.Kind(), tt.msg.Kind())
			}
			if !equalIDPtr(got.ID, tt.msg.ID) {
				t.Errorf("ID = %v, want %v", got.ID, tt.msg.ID)
			}
			if got.Method != tt.msg.Method {
				t.Errorf("Method = %q, want %q", got.Method, tt.msg.Method)
			}
			if !bytes.Equal(got.Params, tt.msg.Params) || !bytes.Equal(got.Result, tt.msg.Result) {
				t.Errorf("Params/Result changed: got %s / %s", got.Params, got.Result)
			}
		})
	}
}

func TestMarshalShape(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
		want string
	}{
		{
			name: "notification omits id",
			msg:  NewNotification("notifications/initialized", nil),
			want: `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		},
		{
			name: "error response without id writes null id",
			msg:  NewErrorResponse(nil, &Error{Code: CodeParseError, Message: "Parse error"}),
			want: `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`,
		},
		{
			name: "string id stays a string",
			msg:  NewResult(NewStringID("7"), json.RawMessage(`{}`)),
			want: `{"jsonrpc":"2.0","id":"7","result":{}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if string(data) != tt.want {
				t.Errorf("Marshal() = %s, want %s", data, tt.want)
			}
		})
	}
}

func TestIDsOfDifferentTypesAreDistinct(t *testing.T) {
	// String "1" and integer 1 are different JSON-RPC IDs. Mixing them up
	// would send one caller's response to another.
	pending := map[ID]string{
		NewIntID(1):      "int",
		NewStringID("1"): "string",
	}
	if len(pending) != 2 {
		t.Fatalf("map has %d entries, want 2", len(pending))
	}
	if pending[NewIntID(1)] != "int" || pending[NewStringID("1")] != "string" {
		t.Errorf("lookups returned the wrong entries: %v", pending)
	}
}

func ptr[T any](v T) *T { return &v }

func equalIDPtr(a, b *ID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
