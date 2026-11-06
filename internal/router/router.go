// Package router is the gateway's MCP server logic for several upstreams. It
// answers server/discover itself, merges tools/list from every upstream with
// each tool name prefixed by its upstream's name, sends tools/call to the
// upstream its prefix names, and rejects methods it cannot route.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
)

// Separator joins an upstream name and a tool name: fs__read_file.
const Separator = "__"

// DefaultMaxPages limits how many tools/list pages are read from one
// upstream, so a server that keeps returning a cursor cannot loop forever.
const DefaultMaxPages = 100

const (
	serverName    = "mcp-gateway"
	serverVersion = "dev"
)

// Upstream is what the router needs from an upstream connection.
// upstream.Conn and upstream.Process satisfy it; tests use fakes.
type Upstream interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
}

// Named is an upstream and the name that prefixes its tools.
type Named struct {
	Name     string
	Upstream Upstream
}

// Options configures a Router. Zero values pick defaults.
type Options struct {
	MaxPages int
	Logger   *slog.Logger
}

// Router answers client requests using several upstreams. It is safe for
// concurrent use.
type Router struct {
	upstreams []Named // in config order, which is the order tools are listed in
	byName    map[string]Upstream
	maxPages  int
	logger    *slog.Logger
}

// New returns a Router over upstreams. Names must be non-empty, unique, and
// free of underscores, so a prefixed tool name splits only one way.
func New(upstreams []Named, opts Options) (*Router, error) {
	if len(upstreams) == 0 {
		return nil, errors.New("router: no upstreams")
	}
	byName := make(map[string]Upstream, len(upstreams))
	for _, u := range upstreams {
		_, duplicate := byName[u.Name]
		switch {
		case u.Name == "":
			return nil, errors.New("router: upstream with empty name")
		case strings.Contains(u.Name, "_"):
			return nil, fmt.Errorf("router: upstream name %q contains '_'", u.Name)
		case duplicate:
			return nil, fmt.Errorf("router: duplicate upstream name %q", u.Name)
		case u.Upstream == nil:
			return nil, fmt.Errorf("router: upstream %q is nil", u.Name)
		}
		byName[u.Name] = u.Upstream
	}

	if opts.MaxPages <= 0 {
		opts.MaxPages = DefaultMaxPages
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Router{
		upstreams: append([]Named(nil), upstreams...),
		byName:    byName,
		maxPages:  opts.MaxPages,
		logger:    opts.Logger,
	}, nil
}

// Call answers one client request. Protocol errors come back as a message
// with Error set and a nil error; a non-nil error means no answer could be
// produced (for example, ctx was cancelled).
func (r *Router) Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error) {
	switch method {
	case "server/discover":
		if rpcErr := mcp.CheckMeta(params); rpcErr != nil {
			return errorMessage(rpcErr), nil
		}
		return r.discover()
	case "tools/list":
		if rpcErr := mcp.CheckMeta(params); rpcErr != nil {
			return errorMessage(rpcErr), nil
		}
		return r.listTools(ctx, params)
	case "tools/call":
		if rpcErr := mcp.CheckMeta(params); rpcErr != nil {
			return errorMessage(rpcErr), nil
		}
		return r.callTool(ctx, params)
	default:
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found: " + method}), nil
	}
}

// Notify handles a client notification. None of the client notifications in
// 2026-07-28 need to reach upstreams yet (the proxy handles cancellation), so
// they are dropped.
func (r *Router) Notify(_ context.Context, method string, _ json.RawMessage) error {
	r.logger.Debug("dropping client notification", "method", method)
	return nil
}

func (r *Router) discover() (*jsonrpc.Message, error) {
	type implementation struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	return resultMessage(struct {
		ResultType        string                    `json:"resultType"`
		SupportedVersions []string                  `json:"supportedVersions"`
		Capabilities      map[string]struct{}       `json:"capabilities"`
		Meta              map[string]implementation `json:"_meta"`
	}{
		ResultType:        "complete",
		SupportedVersions: []string{mcp.ProtocolVersion},
		// No listChanged: the gateway does not send list-changed notifications yet.
		Capabilities: map[string]struct{}{"tools": {}},
		Meta: map[string]implementation{
			"io.modelcontextprotocol/serverInfo": {Name: serverName, Version: serverVersion},
		},
	})
}

type listParams struct {
	Meta   json.RawMessage `json:"_meta"`
	Cursor *string         `json:"cursor,omitempty"`
}

// listTools asks every upstream for all of its tools at once, and returns
// them merged and prefixed in config order. An upstream that fails is left
// out and logged, so one broken server does not hide every tool. If none
// answer, the client gets an error.
func (r *Router) listTools(ctx context.Context, params json.RawMessage) (*jsonrpc.Message, error) {
	var p listParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tools/list params"}), nil
	}
	if p.Cursor != nil {
		// The gateway returns every tool in one page, so it never hands out
		// cursors and none can be valid.
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid cursor"}), nil
	}

	type fetched struct {
		tools []json.RawMessage
		err   error
	}
	results := make([]fetched, len(r.upstreams))
	var wg sync.WaitGroup
	for i, u := range r.upstreams {
		wg.Go(func() {
			results[i].tools, results[i].err = r.fetchTools(ctx, u.Upstream, p.Meta)
		})
	}
	wg.Wait()

	// A cancelled request gets no answer. A timeout still returns whatever
	// the faster upstreams sent, since a partial list is the agreed behavior.
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, fmt.Errorf("router: tools/list: %w", ctx.Err())
	}

	merged := []json.RawMessage{}
	answered := 0
	for i, res := range results {
		name := r.upstreams[i].Name
		if res.err != nil {
			r.logger.Warn("leaving upstream out of tools/list", "upstream", name, "error", res.err)
			continue
		}
		answered++
		merged = append(merged, r.prefixTools(name, res.tools)...)
	}
	if answered == 0 {
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "no upstream answered tools/list"}), nil
	}

	return resultMessage(struct {
		ResultType string            `json:"resultType"`
		Tools      []json.RawMessage `json:"tools"`
	}{"complete", merged})
}

// fetchTools reads every tools/list page from one upstream.
func (r *Router) fetchTools(ctx context.Context, up Upstream, meta json.RawMessage) ([]json.RawMessage, error) {
	var tools []json.RawMessage
	var cursor *string
	for range r.maxPages {
		params, err := json.Marshal(listParams{Meta: meta, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("encode tools/list params: %w", err)
		}
		resp, err := up.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("upstream returned error: %w", resp.Error)
		}

		var page struct {
			ResultType string            `json:"resultType"`
			Tools      []json.RawMessage `json:"tools"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp.Result, &page); err != nil {
			return nil, fmt.Errorf("decode tools/list result: %w", err)
		}
		if page.ResultType != "" && page.ResultType != "complete" {
			return nil, fmt.Errorf("unexpected tools/list resultType %q", page.ResultType)
		}
		tools = append(tools, page.Tools...)

		// Only a missing or null nextCursor ends the list. An empty string
		// is a real cursor.
		if page.NextCursor == nil {
			return tools, nil
		}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("tools/list still had more pages after %d", r.maxPages)
}

// prefixTools renames each tool to prefix__name and leaves every other field
// untouched. A malformed tool is skipped so it cannot hide the others, and a
// repeated name keeps only the first tool.
func (r *Router) prefixTools(prefix string, tools []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for i, raw := range tools {
		name, tool, err := prefixTool(prefix, raw)
		if err != nil {
			r.logger.Warn("skipping malformed tool", "upstream", prefix, "index", i, "error", err)
			continue
		}
		if seen[name] {
			r.logger.Warn("skipping duplicate tool", "upstream", prefix, "tool", name)
			continue
		}
		seen[name] = true
		out = append(out, tool)
	}
	return out
}

func prefixTool(prefix string, raw json.RawMessage) (string, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", nil, errors.New("tool is not a JSON object")
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil || name == "" {
		return "", nil, errors.New("tool has no name")
	}

	full := prefix + Separator + name
	encodedName, err := json.Marshal(full)
	if err != nil {
		return "", nil, fmt.Errorf("encode tool name: %w", err)
	}
	fields["name"] = encodedName
	tool, err := json.Marshal(fields)
	if err != nil {
		return "", nil, fmt.Errorf("encode tool: %w", err)
	}
	return full, tool, nil
}

// callTool sends a tools/call to the upstream named by the tool's prefix,
// with the prefix removed. The upstream's answer comes back unchanged. It is
// never retried, since the tool may have side effects.
func (r *Router) callTool(ctx context.Context, params json.RawMessage) (*jsonrpc.Message, error) {
	// A map keeps every other field (arguments, _meta, requestState, ...)
	// exactly as the client sent it.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tools/call params"}), nil
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil || name == "" {
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tools/call needs a tool name"}), nil
	}

	// Upstream names have no '_', so the prefix always ends at the first "__".
	prefix, tool, found := strings.Cut(name, Separator)
	up, known := r.byName[prefix]
	if !found || !known || tool == "" {
		return errorMessage(&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Unknown tool: " + name}), nil
	}

	encodedTool, err := json.Marshal(tool)
	if err != nil {
		return nil, fmt.Errorf("router: encode tool name: %w", err)
	}
	fields["name"] = encodedTool
	forwarded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("router: encode tools/call params: %w", err)
	}

	resp, err := up.Call(ctx, "tools/call", forwarded)
	if err != nil {
		return nil, fmt.Errorf("router: %s: %w", prefix, err)
	}
	return resp, nil
}

func resultMessage(v any) (*jsonrpc.Message, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("router: encode result: %w", err)
	}
	return &jsonrpc.Message{Result: data}, nil
}

func errorMessage(rpcErr *jsonrpc.Error) *jsonrpc.Message {
	return &jsonrpc.Message{Error: rpcErr}
}
