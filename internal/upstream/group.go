package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
)

// discoverParams is the gateway's own request metadata for the startup check.
var discoverParams = json.RawMessage(`{"_meta":{` +
	`"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `",` +
	`"io.modelcontextprotocol/clientCapabilities":{},` +
	`"io.modelcontextprotocol/clientInfo":{"name":"mcp-gateway","version":"dev"}}}`)

// Spec names an upstream and says how to start it.
type Spec struct {
	Name    string
	Command Command
}

// Member is one running upstream in a Group.
type Member struct {
	Name    string
	Process *Process
}

// Group is a set of upstream processes started together and stopped
// together.
type Group struct {
	members []Member

	done     chan struct{} // closed when the first member exits
	doneOnce sync.Once
	mu       sync.Mutex
	err      error // why the first member exited

	closeOnce sync.Once
	closeErr  error
}

// StartGroup starts every upstream in order and checks each one with a
// server/discover request. If any upstream fails to start or fails the check,
// the ones already started are stopped and an error naming the failed
// upstream is returned, so the gateway never runs with part of its config.
func StartGroup(ctx context.Context, specs []Spec, opts Options) (*Group, error) {
	if len(specs) == 0 {
		return nil, errors.New("upstream: no upstreams to start")
	}
	opts = opts.withDefaults()
	g := &Group{done: make(chan struct{})}

	for _, spec := range specs {
		memberOpts := opts
		memberOpts.Logger = opts.Logger.With("upstream", spec.Name)

		proc, err := Start(spec.Command, memberOpts)
		if err == nil {
			g.members = append(g.members, Member{Name: spec.Name, Process: proc})
			err = checkServer(ctx, proc, opts)
		}
		if err != nil {
			if closeErr := g.Close(); closeErr != nil {
				opts.Logger.Warn("stop upstreams after failed start", "error", closeErr)
			}
			return nil, fmt.Errorf("upstream %q: %w", spec.Name, err)
		}
	}

	for _, m := range g.members {
		go g.watch(m)
	}
	return g, nil
}

// checkServer makes sure a new upstream is alive and speaks our protocol
// version. Without it, a server that crashes on startup would only be
// noticed on the first client request.
func checkServer(ctx context.Context, p *Process, opts Options) error {
	ctx, cancel := context.WithTimeout(ctx, opts.StartupTimeout)
	defer cancel()

	resp, err := p.Call(ctx, "server/discover", discoverParams)
	if err != nil {
		return fmt.Errorf("startup check: server/discover: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("startup check: server/discover: %w", resp.Error)
	}
	var result struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("startup check: server/discover: decode result: %w", err)
	}
	if !slices.Contains(result.SupportedVersions, mcp.ProtocolVersion) {
		return fmt.Errorf("startup check: server supports %v, gateway needs %s", result.SupportedVersions, mcp.ProtocolVersion)
	}
	return nil
}

func (g *Group) watch(m Member) {
	<-m.Process.Done()
	g.doneOnce.Do(func() {
		g.mu.Lock()
		g.err = fmt.Errorf("upstream %q: %w", m.Name, m.Process.Err())
		g.mu.Unlock()
		close(g.done)
	})
}

// Members returns the running upstreams in config order.
func (g *Group) Members() []Member {
	return slices.Clone(g.members)
}

// Done is closed when any member exits, including during Close.
func (g *Group) Done() <-chan struct{} { return g.done }

// Err says which member exited first and why, or nil while all are running.
func (g *Group) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// Close stops every member at the same time, so one slow upstream does not
// delay the others. Calling Close again returns the same result.
func (g *Group) Close() error {
	g.closeOnce.Do(func() {
		errs := make([]error, len(g.members))
		var wg sync.WaitGroup
		for i, m := range g.members {
			wg.Go(func() {
				if err := m.Process.Close(); err != nil {
					errs[i] = fmt.Errorf("upstream %q: %w", m.Name, err)
				}
			})
		}
		wg.Wait()
		g.closeErr = errors.Join(errs...)
	})
	return g.closeErr
}
