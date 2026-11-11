// Package fakeupstream is a small MCP server used in tests. It speaks the
// 2026-07-28 protocol over newline-delimited stdio and exposes a few fake tools.
package fakeupstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// ProtocolVersion is the only MCP version this server supports.
const ProtocolVersion = "2026-07-28"

// CodeUnsupportedProtocolVersion is the MCP error code for a request whose
// protocol version the server does not support.
const CodeUnsupportedProtocolVersion = -32022

const (
	serverName    = "fakeupstream"
	serverVersion = "0.1.0"
)

// Serve answers requests read from in, writing responses to out, until in
// reaches EOF. Requests are handled one at a time, in order. It returns nil on
// a clean EOF and an error if reading or writing fails.
func Serve(in io.Reader, out io.Writer, maxMessageSize int) error {
	r := jsonrpc.NewReader(in, maxMessageSize)
	w := jsonrpc.NewWriter(out)

	for {
		msg, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			resp, fatal := responseForReadError(err)
			if fatal {
				return fmt.Errorf("fakeupstream: read request: %w", err)
			}
			if err := w.Write(resp); err != nil {
				return fmt.Errorf("fakeupstream: write response: %w", err)
			}
			continue
		}

		// Notifications never get a reply, and clients must not send
		// responses in this protocol version, so only requests are answered.
		if msg.Kind() != jsonrpc.KindRequest {
			continue
		}
		// Progress notifications go out before the response, as the spec asks.
		var notifyErr error
		notify := func(n *jsonrpc.Message) {
			if notifyErr == nil {
				notifyErr = w.Write(n)
			}
		}
		resp := handle(msg, notify)
		if notifyErr != nil {
			return fmt.Errorf("fakeupstream: write notification: %w", notifyErr)
		}
		if err := w.Write(resp); err != nil {
			return fmt.Errorf("fakeupstream: write response: %w", err)
		}
	}
}

// responseForReadError turns a bad line into an error response. It reports
// fatal for I/O errors, after which the stream cannot be used.
func responseForReadError(err error) (resp *jsonrpc.Message, fatal bool) {
	var rpcErr *jsonrpc.Error
	switch {
	case errors.As(err, &rpcErr):
		return jsonrpc.NewErrorResponse(nil, rpcErr), false
	case errors.Is(err, jsonrpc.ErrMessageTooLarge):
		return jsonrpc.NewErrorResponse(nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidRequest,
			Message: "message too large",
		}), false
	default:
		return nil, true
	}
}

func handle(req *jsonrpc.Message, notify func(*jsonrpc.Message)) *jsonrpc.Message {
	var result any
	var rpcErr *jsonrpc.Error

	switch req.Method {
	case "server/discover":
		rpcErr = checkMeta(req.Params)
		result = discoverResult()
	case "tools/list":
		rpcErr = checkMeta(req.Params)
		result = toolsListResult()
	case "tools/call":
		if rpcErr = checkMeta(req.Params); rpcErr == nil {
			result, rpcErr = callTool(req.Params, notify)
		}
	case "initialize":
		// The spec asks modern-only servers to name their versions here,
		// since legacy clients have no other way to find out.
		rpcErr = &jsonrpc.Error{
			Code:    jsonrpc.CodeMethodNotFound,
			Message: "initialize is not supported; this server supports protocol version " + ProtocolVersion,
		}
	default:
		rpcErr = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found: " + req.Method}
	}

	if rpcErr != nil {
		return jsonrpc.NewErrorResponse(req.ID, rpcErr)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return jsonrpc.NewErrorResponse(req.ID, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: "encode result: " + err.Error(),
		})
	}
	return jsonrpc.NewResult(*req.ID, data)
}

// requestMeta holds the per-request fields that 2026-07-28 requires in _meta.
type requestMeta struct {
	ProtocolVersion    *string         `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

// checkMeta validates the required _meta fields. A missing or malformed field
// is invalid params (-32602); a well-formed but unknown version gets -32022.
func checkMeta(params json.RawMessage) *jsonrpc.Error {
	if params == nil {
		return invalidParams("missing params with required _meta")
	}
	var p struct {
		Meta *requestMeta `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return invalidParams("invalid _meta: " + err.Error())
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
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "encode error data: " + err.Error()}
	}
	return &jsonrpc.Error{Code: CodeUnsupportedProtocolVersion, Message: "Unsupported protocol version", Data: data}
}

func invalidParams(msg string) *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type discoverResultBody struct {
	ResultType        string                    `json:"resultType"`
	SupportedVersions []string                  `json:"supportedVersions"`
	Capabilities      map[string]struct{}       `json:"capabilities"`
	Meta              map[string]implementation `json:"_meta"`
}

func discoverResult() discoverResultBody {
	return discoverResultBody{
		ResultType:        "complete",
		SupportedVersions: []string{ProtocolVersion},
		Capabilities:      map[string]struct{}{"tools": {}},
		Meta: map[string]implementation{
			"io.modelcontextprotocol/serverInfo": {Name: serverName, Version: serverVersion},
		},
	}
}
