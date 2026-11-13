// Package httpfront serves MCP over Streamable HTTP as defined for protocol
// version 2026-07-28: every client message is its own POST to one endpoint,
// there are no sessions, and closing the connection cancels the request.
package httpfront

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/progress"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/proxy"
)

// Path is the MCP endpoint.
const Path = "/mcp"

// Authenticator maps an API key to an agent. *auth.Authenticator does.
type Authenticator interface {
	Authenticate(key string) (auth.Agent, error)
}

// Options configures the handler. Zero values pick defaults.
type Options struct {
	// Auth checks the Bearer key on every request. If nil, every request is
	// rejected: a missing authenticator must never mean "no auth".
	Auth Authenticator
	// AllowedOrigins lists the exact Origin header values accepted. A request
	// with any other Origin is rejected; requests without one are allowed.
	// Empty means every request that sends an Origin is rejected.
	AllowedOrigins []string
	// CallTimeout limits each request.
	CallTimeout time.Duration
	// MaxMessageSize limits each request body.
	MaxMessageSize int
	Logger         *slog.Logger
	// Progress routes upstream progress notifications back to the request
	// that asked for them, over an SSE stream. If nil, replies are always
	// plain JSON.
	Progress *progress.Tracker
	// Schemas provides tool input schemas, used to check Mcp-Param-*
	// headers on tools/call. If nil, those headers are not checked.
	Schemas SchemaSource
}

// SchemaSource looks up a tool's inputSchema. router.Router implements it.
type SchemaSource interface {
	ToolSchema(ctx context.Context, tool string, meta json.RawMessage) (schema json.RawMessage, found bool, err error)
}

type frontend struct {
	auth           Authenticator
	handler        proxy.Handler
	allowedOrigins map[string]bool
	callTimeout    time.Duration
	maxMessageSize int
	logger         *slog.Logger
	progress       *progress.Tracker
	schemas        SchemaSource
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
		auth:           opts.Auth,
		handler:        h,
		allowedOrigins: make(map[string]bool, len(opts.AllowedOrigins)),
		callTimeout:    opts.CallTimeout,
		maxMessageSize: opts.MaxMessageSize,
		logger:         opts.Logger,
		progress:       opts.Progress,
		schemas:        opts.Schemas,
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
	agent, err := f.authenticate(r)
	if err != nil {
		// Missing and wrong keys get the same answer, so the response does not
		// tell a caller which keys exist. The log keeps the reason.
		f.logger.Warn("rejecting unauthenticated request", "reason", err, "remote", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		f.writeMessage(w, http.StatusUnauthorized, jsonrpc.NewErrorResponse(nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "unauthorized"}))
		return
	}
	// From here on the agent travels in the context. The key itself goes no
	// further: it is never passed to the router or any upstream.
	r = r.WithContext(auth.WithAgent(r.Context(), agent))

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

// authenticate reads "Authorization: Bearer <key>". Keys in the URL are
// never read, as the spec forbids them there.
func (f *frontend) authenticate(r *http.Request) (auth.Agent, error) {
	if f.auth == nil {
		return auth.Agent{}, errors.New("no authenticator configured")
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		return auth.Agent{}, auth.ErrNoKey
	}
	key, ok := auth.ParseBearer(header)
	if !ok {
		return auth.Agent{}, fmt.Errorf("%w: Authorization header is not a Bearer token", auth.ErrInvalidKey)
	}
	return f.auth.Authenticate(key)
}

func (f *frontend) serveRequest(w http.ResponseWriter, r *http.Request, req *jsonrpc.Message) {
	if rpcErr := checkHeaders(r.Header, req); rpcErr != nil {
		f.writeMessage(w, http.StatusBadRequest, jsonrpc.NewErrorResponse(req.ID, rpcErr))
		return
	}
	if req.Method == "tools/call" && f.schemas != nil {
		if status, rpcErr := f.checkToolParams(r.Context(), r.Header, req); rpcErr != nil {
			f.writeMessage(w, status, jsonrpc.NewErrorResponse(req.ID, rpcErr))
			return
		}
	}
	// The SSE stream only starts if progress actually arrives. Until then the
	// reply can still be plain JSON with the status code its error calls for.
	sse := &sseWriter{w: w, logger: f.logger}
	// r.Context() is cancelled when the client disconnects, which is how
	// Streamable HTTP cancels a request.
	proxy.ForwardWithProgress(r.Context(), f.handler, f.callTimeout, req, f.logger, f.progress, sse.event, func(resp *jsonrpc.Message) {
		switch {
		case resp == nil:
			// The client is gone.
		case sse.started:
			sse.event(resp)
		default:
			f.writeMessage(w, statusFor(resp.Error), resp)
		}
	})
}

// checkToolParams checks Mcp-Param-* headers against the call's arguments,
// using the tool's schema. It fails closed: if the schema cannot be fetched or
// the tool definition is invalid, the call is rejected, since the headers
// cannot be shown to match.
func (f *frontend) checkToolParams(ctx context.Context, h http.Header, req *jsonrpc.Message) (int, *jsonrpc.Error) {
	var params struct {
		Name string          `json:"name"`
		Meta json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return 0, nil // the router rejects malformed params with its own error
	}
	schema, found, err := f.schemas.ToolSchema(ctx, params.Name, params.Meta)
	if err != nil {
		f.logger.Warn("tool schema lookup failed", "tool", params.Name, "error", err)
		return http.StatusOK, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "could not check Mcp-Param headers"}
	}
	if !found {
		return 0, nil // the router or upstream answers "Unknown tool"
	}
	if err := checkParamHeaders(h, schema, req.Params); err != nil {
		f.logger.Debug("Mcp-Param header check failed", "tool", params.Name, "error", err)
		return http.StatusBadRequest, mismatch(err.Error())
	}
	return 0, nil
}

// sseWriter writes messages as Server-Sent Events. It is only used through a
// progress.Stream, which keeps calls from overlapping.
type sseWriter struct {
	w       http.ResponseWriter
	logger  *slog.Logger
	started bool
}

func (s *sseWriter) event(msg *jsonrpc.Message) {
	if !s.started {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		// Tells proxies like nginx not to hold events back (spec SHOULD).
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.logger.Error("encode SSE event", "error", err)
		return
	}
	// One JSON message per event. Marshal output has no newlines, so it fits
	// on one data line.
	if _, err := fmt.Fprintf(s.w, "event: message\ndata: %s\n\n", data); err != nil {
		s.logger.Debug("write SSE event", "error", err)
		return
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		s.logger.Debug("flush SSE event", "error", err)
	}
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
