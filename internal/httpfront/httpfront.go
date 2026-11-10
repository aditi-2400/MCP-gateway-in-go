// Package httpfront serves MCP over Streamable HTTP as defined for protocol
// version 2026-07-28: every client message is its own POST to one endpoint,
// there are no sessions, and closing the connection cancels the request.
package httpfront

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/proxy"
)

// Path is the MCP endpoint.
const Path = "/mcp"

// Options configures the handler. Zero values pick defaults.
type Options struct {
	// AllowedOrigins lists the exact Origin header values accepted. A request
	// with any other Origin is rejected; requests without one are allowed.
	// Empty means every request that sends an Origin is rejected.
	AllowedOrigins []string
	// CallTimeout limits each request.
	CallTimeout time.Duration
	// MaxMessageSize limits each request body.
	MaxMessageSize int
	Logger         *slog.Logger
}

type frontend struct {
	handler        proxy.Handler
	allowedOrigins map[string]bool
	callTimeout    time.Duration
	maxMessageSize int
	logger         *slog.Logger
}

// New returns an http.Handler that serves MCP at Path and passes each
// message to h. Other paths get 404.
func New(h proxy.Handler, opts Options) http.Handler {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = proxy.DefaultCallTimeout
	}
	if opts.MaxMessageSize <= 0 {
		opts.MaxMessageSize = jsonrpc.DefaultMaxMessageSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	f := &frontend{
		handler:        h,
		allowedOrigins: make(map[string]bool, len(opts.AllowedOrigins)),
		callTimeout:    opts.CallTimeout,
		maxMessageSize: opts.MaxMessageSize,
		logger:         opts.Logger,
	}
	for _, origin := range opts.AllowedOrigins {
		f.allowedOrigins[origin] = true
	}

	mux := http.NewServeMux()
	mux.Handle(Path, f)
	return mux
}

func (f *frontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 2026-07-28 removed the GET stream and DELETE session endpoints.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// The spec requires this check to stop DNS rebinding: a web page that
	// tricks a browser into calling a local gateway still sends its own Origin.
	if origin := r.Header.Get("Origin"); origin != "" && !f.allowedOrigins[origin] {
		f.logger.Warn("rejecting request from origin not on the allowlist", "origin", origin)
		f.writeMessage(w, http.StatusForbidden, jsonrpc.NewErrorResponse(nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "origin not allowed"}))
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(f.maxMessageSize)))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		f.logger.Debug("read request body", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg, err := jsonrpc.Parse(body)
	if err != nil {
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "invalid message"}
		}
		f.writeMessage(w, http.StatusBadRequest, jsonrpc.NewErrorResponse(nil, rpcErr))
		return
	}

	switch msg.Kind() {
	case jsonrpc.KindNotification:
		if err := f.handler.Notify(r.Context(), msg.Method, msg.Params); err != nil {
			f.logger.Warn("client notification", "method", msg.Method, "error", err)
		}
		w.WriteHeader(http.StatusAccepted)
	case jsonrpc.KindRequest:
		f.serveRequest(w, r, msg)
	default:
		f.writeMessage(w, http.StatusBadRequest, jsonrpc.NewErrorResponse(nil, &jsonrpc.Error{
			Code: jsonrpc.CodeInvalidRequest, Message: "clients must not send responses",
		}))
	}
}

func (f *frontend) serveRequest(w http.ResponseWriter, r *http.Request, req *jsonrpc.Message) {
	if rpcErr := checkHeaders(r.Header, req); rpcErr != nil {
		f.writeMessage(w, http.StatusBadRequest, jsonrpc.NewErrorResponse(req.ID, rpcErr))
		return
	}
	// r.Context() is cancelled when the client disconnects, which is how
	// Streamable HTTP cancels a request.
	resp := proxy.Forward(r.Context(), f.handler, f.callTimeout, req, f.logger)
	if resp == nil {
		return // the client is gone
	}
	f.writeMessage(w, statusFor(resp.Error), resp)
}

// statusFor picks the HTTP status the spec asks for. Other errors, such as
// an unknown tool, are normal JSON-RPC answers with 200.
func statusFor(rpcErr *jsonrpc.Error) int {
	if rpcErr == nil {
		return http.StatusOK
	}
	switch rpcErr.Code {
	case jsonrpc.CodeMethodNotFound:
		return http.StatusNotFound
	case mcp.CodeUnsupportedProtocolVersion, mcp.CodeHeaderMismatch:
		return http.StatusBadRequest
	default:
		return http.StatusOK
	}
}

// checkHeaders makes sure the MCP headers match the body, so a proxy or load
// balancer routing on headers can never see a different request than the
// gateway runs. Mcp-Param-* headers are not checked yet (see PROGRESS.md).
func checkHeaders(h http.Header, req *jsonrpc.Message) *jsonrpc.Error {
	var params struct {
		Name *string `json:"name"`
		URI  *string `json:"uri"`
		Meta *struct {
			ProtocolVersion *string `json:"io.modelcontextprotocol/protocolVersion"`
		} `json:"_meta"`
	}
	// A malformed body is left for the router to reject with its own error.
	_ = json.Unmarshal(req.Params, &params)

	version := h.Get("MCP-Protocol-Version")
	if version == "" {
		return mismatch("missing MCP-Protocol-Version header")
	}
	if params.Meta != nil && params.Meta.ProtocolVersion != nil && *params.Meta.ProtocolVersion != version {
		return mismatch("MCP-Protocol-Version header does not match _meta")
	}

	method := h.Get("Mcp-Method")
	if method == "" {
		return mismatch("missing Mcp-Method header")
	}
	if method != req.Method {
		return mismatch("Mcp-Method header does not match method")
	}

	var bodyName *string
	switch req.Method {
	case "tools/call", "prompts/get":
		bodyName = params.Name
	case "resources/read":
		bodyName = params.URI
	default:
		return nil
	}
	headerName := h.Get("Mcp-Name")
	if headerName == "" {
		return mismatch("missing Mcp-Name header")
	}
	decoded, err := decodeHeaderValue(headerName)
	if err != nil {
		return mismatch("Mcp-Name header has invalid base64")
	}
	if bodyName == nil || decoded != *bodyName {
		return mismatch("Mcp-Name header does not match the request body")
	}
	return nil
}

// decodeHeaderValue undoes the spec's =?base64?...?= encoding, used for
// values that are not plain ASCII.
func decodeHeaderValue(v string) (string, error) {
	const prefix, suffix = "=?base64?", "?="
	if !strings.HasPrefix(v, prefix) || !strings.HasSuffix(v, suffix) || len(v) < len(prefix)+len(suffix) {
		return v, nil
	}
	data, err := base64.StdEncoding.DecodeString(v[len(prefix) : len(v)-len(suffix)])
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func mismatch(msg string) *jsonrpc.Error {
	return &jsonrpc.Error{Code: mcp.CodeHeaderMismatch, Message: "Header mismatch: " + msg}
}

func (f *frontend) writeMessage(w http.ResponseWriter, status int, msg *jsonrpc.Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		f.logger.Error("encode response", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		f.logger.Debug("write response", "error", err)
	}
}
