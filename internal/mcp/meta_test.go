package mcp

import (
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

func TestCheckMeta(t *testing.T) {
	tests := []struct {
		name     string
		params   string // "" means no params at all
		wantCode int    // 0 means valid
		wantData string
	}{
		{name: "valid", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`},
		{name: "valid with other params", params: `{"name":"x","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"a":1}}}`},
		{name: "no params", params: "", wantCode: jsonrpc.CodeInvalidParams},
		{name: "no _meta", params: `{}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "_meta is null", params: `{"_meta":null}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "_meta is a string", params: `{"_meta":"x"}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "missing version", params: `{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "version is a number", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":2026,"io.modelcontextprotocol/clientCapabilities":{}}}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "missing capabilities", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "capabilities is an array", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":[]}}`, wantCode: jsonrpc.CodeInvalidParams},
		{name: "capabilities is null", params: `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":null}}`, wantCode: jsonrpc.CodeInvalidParams},
		{
			name:     "old version",
			params:   `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}`,
			wantCode: CodeUnsupportedProtocolVersion,
			wantData: `{"supported":["2026-07-28"],"requested":"2025-11-25"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params []byte
			if tt.params != "" {
				params = []byte(tt.params)
			}
			err := CheckMeta(params)
			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("CheckMeta() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Code != tt.wantCode {
				t.Fatalf("CheckMeta() = %v, want code %d", err, tt.wantCode)
			}
			if tt.wantData != "" && string(err.Data) != tt.wantData {
				t.Errorf("data = %s, want %s", err.Data, tt.wantData)
			}
		})
	}
}
