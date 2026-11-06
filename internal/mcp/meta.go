// Package mcp holds MCP protocol constants and checks for the gateway's
// client-facing side.
package mcp

import (
	"encoding/json"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// ProtocolVersion is the only MCP version the gateway speaks.
const ProtocolVersion = "2026-07-28"

// CodeUnsupportedProtocolVersion is the error for a well-formed but unknown
// protocol version in a request's _meta.
const CodeUnsupportedProtocolVersion = -32022

// RequestMeta holds the per-request fields that 2026-07-28 requires in _meta.
type RequestMeta struct {
	ProtocolVersion    *string         `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

// CheckMeta validates the required _meta fields of a request's params. A
// missing or malformed field is invalid params (-32602), and a well-formed
// but unsupported version is -32022 with the supported versions in data.
func CheckMeta(params json.RawMessage) *jsonrpc.Error {
	if params == nil {
		return invalidParams("missing params with required _meta")
	}
	var p struct {
		Meta *RequestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return invalidParams("invalid _meta")
	}
	if p.Meta == nil {
		return invalidParams("missing _meta")
	}
	if p.Meta.ProtocolVersion == nil {
		return invalidParams("missing _meta.io.modelcontextprotocol/protocolVersion")
	}
	if len(p.Meta.ClientCapabilities) == 0 || p.Meta.ClientCapabilities[0] != '{' {
		return invalidParams("_meta.io.modelcontextprotocol/clientCapabilities must be an object")
	}
	if *p.Meta.ProtocolVersion != ProtocolVersion {
		return unsupportedVersion(*p.Meta.ProtocolVersion)
	}
	return nil
}

func unsupportedVersion(requested string) *jsonrpc.Error {
	data, err := json.Marshal(struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}{[]string{ProtocolVersion}, requested})
	if err != nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "encode error data"}
	}
	return &jsonrpc.Error{Code: CodeUnsupportedProtocolVersion, Message: "Unsupported protocol version", Data: data}
}

func invalidParams(msg string) *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
}
