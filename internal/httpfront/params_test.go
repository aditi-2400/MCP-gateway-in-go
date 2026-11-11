package httpfront

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
)

func TestParamHeadersFromSchema(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		want    string // "Name=path.to.prop" entries, sorted by name; "" for none
		wantErr string
	}{
		{name: "no annotations", schema: `{"type":"object","properties":{"q":{"type":"string"}}}`, want: ""},
		{name: "top level", schema: `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`, want: "Region=region"},
		{
			name:   "all allowed types and nesting",
			schema: `{"type":"object","properties":{"limit":{"type":"integer","x-mcp-header":"Limit"},"on":{"type":"boolean","x-mcp-header":"On"},"filter":{"type":"object","properties":{"owner":{"type":"string","x-mcp-header":"Owner"}}}}}`,
			want:   "Limit=limit On=on Owner=filter.owner",
		},
		{name: "empty name", schema: `{"properties":{"a":{"type":"string","x-mcp-header":""}}}`, wantErr: "empty"},
		{name: "space in name", schema: `{"properties":{"a":{"type":"string","x-mcp-header":"Re gion"}}}`, wantErr: "token"},
		{name: "colon in name", schema: `{"properties":{"a":{"type":"string","x-mcp-header":"A:B"}}}`, wantErr: "token"},
		{name: "not a string", schema: `{"properties":{"a":{"type":"string","x-mcp-header":7}}}`, wantErr: "string"},
		{name: "duplicate ignoring case", schema: `{"properties":{"a":{"type":"string","x-mcp-header":"Region"},"b":{"type":"string","x-mcp-header":"region"}}}`, wantErr: "unique"},
		{name: "number type", schema: `{"properties":{"a":{"type":"number","x-mcp-header":"A"}}}`, wantErr: "type"},
		{name: "object type", schema: `{"properties":{"a":{"type":"object","x-mcp-header":"A"}}}`, wantErr: "type"},
		{name: "no type", schema: `{"properties":{"a":{"x-mcp-header":"A"}}}`, wantErr: "type"},
		{name: "inside items", schema: `{"properties":{"list":{"type":"array","items":{"type":"string","x-mcp-header":"Item"}}}}`, wantErr: "reachable"},
		{name: "inside oneOf", schema: `{"properties":{"a":{"oneOf":[{"type":"string","x-mcp-header":"A"}]}}}`, wantErr: "reachable"},
		{name: "inside $defs", schema: `{"properties":{"a":{"$ref":"#/$defs/x"}},"$defs":{"x":{"type":"string","x-mcp-header":"A"}}}`, wantErr: "reachable"},
		{name: "on the root", schema: `{"type":"object","x-mcp-header":"Root"}`, wantErr: "reachable"},
		{name: "not an object", schema: `true`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paramHeaders(json.RawMessage(tt.schema))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("paramHeaders() error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("paramHeaders() error = %v", err)
			}
			var parts []string
			for _, p := range got {
				parts = append(parts, p.name+"="+strings.Join(p.path, "."))
			}
			if joined := strings.Join(parts, " "); joined != tt.want {
				t.Fatalf("paramHeaders() = %q, want %q", joined, tt.want)
			}
		})
	}
}

const lookupSchema = `{"type":"object","properties":{` +
	`"region":{"type":"string","x-mcp-header":"Region"},` +
	`"limit":{"type":"integer","x-mcp-header":"Limit"},` +
	`"verbose":{"type":"boolean","x-mcp-header":"Verbose"},` +
	`"filter":{"type":"object","properties":{"owner":{"type":"string","x-mcp-header":"Owner"}}}}}`

func TestCheckParamHeaders(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		headers map[string]string
		ok      bool
	}{
		{name: "string matches", args: `{"region":"us-west1"}`, headers: map[string]string{"Mcp-Param-Region": "us-west1"}, ok: true},
		{name: "base64 string matches", args: `{"region":"日本"}`, headers: map[string]string{"Mcp-Param-Region": b64("日本")}, ok: true},
		{name: "lowercase header name", args: `{"region":"x"}`, headers: map[string]string{"mcp-param-region": "x"}, ok: true},
		{name: "no arguments, no headers", args: `{}`, ok: true},
		{name: "null value, no header", args: `{"region":null}`, ok: true},
		{name: "missing arguments object", args: ``, ok: true},
		{name: "integer matches", args: `{"region":"r","limit":42}`, headers: map[string]string{"Mcp-Param-Region": "r", "Mcp-Param-Limit": "42"}, ok: true},
		{name: "integer compared as a number", args: `{"region":"r","limit":42}`, headers: map[string]string{"Mcp-Param-Region": "r", "Mcp-Param-Limit": "42.0"}, ok: true},
		{name: "boolean matches", args: `{"verbose":true}`, headers: map[string]string{"Mcp-Param-Verbose": "true"}, ok: true},
		{name: "nested value matches", args: `{"filter":{"owner":"ana"}}`, headers: map[string]string{"Mcp-Param-Owner": "ana"}, ok: true},
		{name: "unrecognized param header is ignored", args: `{}`, headers: map[string]string{"Mcp-Param-Other": "x"}, ok: true},
		{name: "value without header", args: `{"region":"us-west1"}`},
		{name: "header without value", args: `{}`, headers: map[string]string{"Mcp-Param-Region": "us-west1"}},
		{name: "header for a null value", args: `{"region":null}`, headers: map[string]string{"Mcp-Param-Region": "null"}},
		{name: "string differs", args: `{"region":"us-west1"}`, headers: map[string]string{"Mcp-Param-Region": "eu-west1"}},
		{name: "string differs in case", args: `{"region":"us-west1"}`, headers: map[string]string{"Mcp-Param-Region": "US-WEST1"}},
		{name: "base64 differs", args: `{"region":"a"}`, headers: map[string]string{"Mcp-Param-Region": b64("b")}},
		{name: "broken base64", args: `{"region":"a"}`, headers: map[string]string{"Mcp-Param-Region": "=?base64?***?="}},
		{name: "integer differs", args: `{"limit":42}`, headers: map[string]string{"Mcp-Param-Limit": "43"}},
		{name: "integer header not a number", args: `{"limit":42}`, headers: map[string]string{"Mcp-Param-Limit": "forty-two"}},
		{name: "boolean differs in case", args: `{"verbose":true}`, headers: map[string]string{"Mcp-Param-Verbose": "True"}},
		{name: "boolean differs", args: `{"verbose":false}`, headers: map[string]string{"Mcp-Param-Verbose": "true"}},
		{name: "nested value without header", args: `{"filter":{"owner":"ana"}}`},
		{name: "object where a string belongs, with header", args: `{"region":{"x":1}}`, headers: map[string]string{"Mcp-Param-Region": "{\"x\":1}"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			var params json.RawMessage
			if tt.args == "" {
				params = json.RawMessage(`{"name":"t"}`)
			} else {
				params = json.RawMessage(`{"name":"t","arguments":` + tt.args + `}`)
			}
			err := checkParamHeaders(h, json.RawMessage(lookupSchema), params)
			if tt.ok && err != nil {
				t.Fatalf("checkParamHeaders() = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("checkParamHeaders() = nil, want a mismatch")
			}
		})
	}
}

type fakeSchemas struct {
	schema string
	found  bool
	err    error
}

func (f fakeSchemas) ToolSchema(context.Context, string, json.RawMessage) (json.RawMessage, bool, error) {
	return json.RawMessage(f.schema), f.found, f.err
}

func TestToolCallParamHeaderChecks(t *testing.T) {
	call := request("1", "tools/call", `"name":"fs__lookup","arguments":{"region":"us-west1"},`+validMeta)
	tests := []struct {
		name       string
		schemas    fakeSchemas
		headers    map[string]string
		wantStatus int
		wantCode   int // 0 means the handler answers
	}{
		{name: "matching header", schemas: fakeSchemas{schema: lookupSchema, found: true}, headers: map[string]string{"Mcp-Param-Region": "us-west1"}, wantStatus: http.StatusOK},
		{name: "missing header", schemas: fakeSchemas{schema: lookupSchema, found: true}, wantStatus: http.StatusBadRequest, wantCode: mcp.CodeHeaderMismatch},
		{name: "wrong header", schemas: fakeSchemas{schema: lookupSchema, found: true}, headers: map[string]string{"Mcp-Param-Region": "eu"}, wantStatus: http.StatusBadRequest, wantCode: mcp.CodeHeaderMismatch},
		{name: "unknown tool is left to the router", schemas: fakeSchemas{found: false}, wantStatus: http.StatusOK},
		{name: "invalid tool definition fails closed", schemas: fakeSchemas{schema: `{"properties":{"region":{"type":"number","x-mcp-header":"Region"}}}`, found: true}, headers: map[string]string{"Mcp-Param-Region": "us-west1"}, wantStatus: http.StatusBadRequest, wantCode: mcp.CodeHeaderMismatch},
		{name: "schema lookup error fails closed", schemas: fakeSchemas{err: errors.New("upstream down")}, headers: map[string]string{"Mcp-Param-Region": "us-west1"}, wantStatus: http.StatusOK, wantCode: jsonrpc.CodeInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			srv := newServer(t, h, Options{Schemas: tt.schemas})
			headers := map[string]string{"Mcp-Name": "fs__lookup"}
			for k, v := range tt.headers {
				headers[k] = v
			}
			resp := post(t, srv, "tools/call", call, headers)
			if tt.wantCode == 0 {
				wantStatus(t, resp, tt.wantStatus)
				if h.callCount() != 1 {
					t.Errorf("handler called %d times, want 1", h.callCount())
				}
				return
			}
			msg := wantRPCError(t, resp, tt.wantStatus, tt.wantCode)
			if strings.Contains(msg.Error.Message, "upstream down") {
				t.Errorf("error %q leaks upstream details", msg.Error.Message)
			}
			if h.callCount() != 0 {
				t.Errorf("handler called %d times, want 0", h.callCount())
			}
		})
	}
}
