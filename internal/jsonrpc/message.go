// Package jsonrpc implements the JSON-RPC 2.0 message layer used by MCP.
package jsonrpc

import (
	"bytes"
	"encoding/json"
)

// Version is the only JSON-RPC version MCP uses.
const Version = "2.0"

// Kind says which of the JSON-RPC message types a Message is.
type Kind int

const (
	KindRequest Kind = iota + 1
	KindNotification
	KindResponse
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindNotification:
		return "notification"
	case KindResponse:
		return "response"
	default:
		return "unknown"
	}
}

// Message is any JSON-RPC message: a request, a notification, or a response.
//
// Params and Result hold the raw JSON bytes so the gateway can forward them
// without decoding and re-encoding them.
type Message struct {
	// ID is nil for notifications, and for error responses to a request
	// whose ID could not be read.
	ID     *ID
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  *Error
}

// Kind reports the message type. It assumes the message is valid, which is
// always true for messages from Parse or the New* constructors.
func (m *Message) Kind() Kind {
	switch {
	case m.Method != "" && m.ID != nil:
		return KindRequest
	case m.Method != "":
		return KindNotification
	default:
		return KindResponse
	}
}

// NewRequest builds a request. params may be nil.
func NewRequest(id ID, method string, params json.RawMessage) *Message {
	return &Message{ID: &id, Method: method, Params: params}
}

// NewNotification builds a notification. params may be nil.
func NewNotification(method string, params json.RawMessage) *Message {
	return &Message{Method: method, Params: params}
}

// NewResult builds a successful response.
func NewResult(id ID, result json.RawMessage) *Message {
	return &Message{ID: &id, Result: result}
}

// NewErrorResponse builds an error response. id may be nil when the request's
// ID could not be read, in which case "id": null is written.
func NewErrorResponse(id *ID, rpcErr *Error) *Message {
	return &Message{ID: id, Error: rpcErr}
}

// wireMessage is the JSON shape on the wire. Fields that need presence checks
// are raw or pointers so "missing" and "null" can be told apart.
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  *string         `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// MarshalJSON writes the message with "jsonrpc":"2.0". Responses always carry
// an id, written as null when ID is nil, as JSON-RPC 2.0 requires.
func (m Message) MarshalJSON() ([]byte, error) {
	w := wireMessage{
		JSONRPC: Version,
		Params:  m.Params,
		Result:  m.Result,
		Error:   m.Error,
	}
	if m.Method != "" {
		w.Method = &m.Method
	}
	switch {
	case m.ID != nil:
		idJSON, err := m.ID.MarshalJSON()
		if err != nil {
			return nil, err
		}
		w.ID = idJSON
	case m.Method == "":
		w.ID = json.RawMessage("null")
	}
	return json.Marshal(w)
}

// Parse decodes and validates one JSON-RPC message from untrusted input.
// On failure it returns a *Error with CodeParseError for invalid JSON or
// CodeInvalidRequest for valid JSON that is not a valid message.
func Parse(data []byte) (*Message, error) {
	if !json.Valid(data) {
		return nil, newError(CodeParseError, "invalid JSON")
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if trimmed[0] != '{' {
		// This also rejects JSON-RPC batches (arrays), which MCP does not use.
		return nil, newError(CodeInvalidRequest, "message must be a JSON object")
	}

	var w wireMessage
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, newError(CodeInvalidRequest, "invalid message: %v", err)
	}
	if w.JSONRPC != Version {
		return nil, newError(CodeInvalidRequest, `"jsonrpc" must be "2.0"`)
	}

	msg := &Message{Params: w.Params, Result: w.Result, Error: w.Error}
	idIsNull := bytes.Equal(w.ID, []byte("null"))
	if len(w.ID) > 0 && !idIsNull {
		var id ID
		if err := id.UnmarshalJSON(w.ID); err != nil {
			return nil, newError(CodeInvalidRequest, "invalid id: %v", err)
		}
		msg.ID = &id
	}

	if w.Method != nil {
		msg.Method = *w.Method
		if err := validateRequestOrNotification(msg, idIsNull); err != nil {
			return nil, err
		}
		return msg, nil
	}
	if err := validateResponse(msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func validateRequestOrNotification(msg *Message, idIsNull bool) *Error {
	if msg.Method == "" {
		return newError(CodeInvalidRequest, `"method" must not be empty`)
	}
	if idIsNull {
		return newError(CodeInvalidRequest, "request id must not be null")
	}
	if msg.Result != nil || msg.Error != nil {
		return newError(CodeInvalidRequest, `a request must not have "result" or "error"`)
	}
	// MCP defines params as an object. Rejecting other shapes here means
	// later code that reads params (policy checks) only sees objects.
	if msg.Params != nil && msg.Params[0] != '{' {
		return newError(CodeInvalidRequest, `"params" must be an object`)
	}
	return nil
}

func validateResponse(msg *Message) *Error {
	hasResult := msg.Result != nil
	hasError := msg.Error != nil
	if hasResult == hasError {
		return newError(CodeInvalidRequest, `a response must have exactly one of "result" or "error"`)
	}
	if hasResult && msg.ID == nil {
		return newError(CodeInvalidRequest, "a result response must have an id")
	}
	return nil
}
