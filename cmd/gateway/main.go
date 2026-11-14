// Command gateway is an MCP gateway. It speaks MCP to one client over its own
// stdin and stdout, and routes requests to the upstream MCP servers listed in
// its config file, each started as a child process.
//
// Usage:
//
//	gateway -config gateway.yaml [-call-timeout 60s] [-log-level info]
//	gateway -config gateway.yaml -http 127.0.0.1:8080 [-allow-origin URL]...
//	gateway keygen -agent NAME
//
// Every caller needs an API key from keygen. Over stdio it goes in the
// MCP_GATEWAY_API_KEY environment variable; over HTTP, in an
// "Authorization: Bearer" header on every request.
//
// Without -http it speaks MCP over stdin and stdout, and logs go to stderr
// because stdout carries MCP messages. With -http it serves Streamable HTTP
// at /mcp and does not read stdin.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/config"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/progress"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/proxy"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/router"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

// errUsage means the command line was wrong. The message and usage text
// have already been printed.
var errUsage = errors.New("usage error")

// errUpstreamExited means an upstream server stopped while the client was
// still connected.
var errUpstreamExited = errors.New("upstream server exited")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()

	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case errors.Is(err, errUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

type flags struct {
	configPath     string
	callTimeout    time.Duration
	logLevel       slog.Level
	httpAddr       string // empty means stdio
	allowedOrigins stringList
	tlsCert        string
	tlsKey         string
	behindTLSProxy bool
}

// stringList is a flag that can be given more than once.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func parseFlags(args []string, stderr io.Writer) (flags, error) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gateway -config gateway.yaml [flags]")
		fs.PrintDefaults()
	}

	f := flags{logLevel: slog.LevelInfo}
	fs.StringVar(&f.configPath, "config", "", "path to the YAML config file listing upstream servers (required)")
	fs.DurationVar(&f.callTimeout, "call-timeout", proxy.DefaultCallTimeout, "how long each request may wait for the upstreams")
	fs.TextVar(&f.logLevel, "log-level", f.logLevel, "log level: debug, info, warn, or error")
	fs.StringVar(&f.httpAddr, "http", "", "serve Streamable HTTP on this address (e.g. 127.0.0.1:8080) in place of stdio; non-loopback addresses need TLS or -behind-tls-proxy")
	fs.Var(&f.allowedOrigins, "allow-origin", "browser Origin allowed to call the HTTP endpoint (repeatable)")
	fs.StringVar(&f.tlsCert, "tls-cert", "", "PEM certificate file; serve HTTPS (needs -tls-key)")
	fs.StringVar(&f.tlsKey, "tls-key", "", "PEM private key file for -tls-cert")
	fs.BoolVar(&f.behindTLSProxy, "behind-tls-proxy", false, "allow plain HTTP on a non-loopback address because a proxy in front terminates TLS")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags{}, err
		}
		return flags{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.configPath == "" {
		return flags{}, usageFailure(fs, "missing -config")
	}
	if fs.NArg() > 0 {
		return flags{}, usageFailure(fs, "unexpected arguments; upstreams are listed in the -config file")
	}
	if f.callTimeout <= 0 {
		return flags{}, usageFailure(fs, "-call-timeout must be positive")
	}
	if err := checkHTTPFlags(f); err != nil {
		return flags{}, usageFailure(fs, err.Error())
	}
	return f, nil
}

func usageFailure(fs *flag.FlagSet, msg string) error {
	fmt.Fprintln(fs.Output(), "gateway:", msg)
	fs.Usage()
	return fmt.Errorf("%w: %s", errUsage, msg)
}

// run is the whole program, minus process exit codes, so tests can drive it.
// It returns nil when the client closes stdin or ctx is cancelled (Ctrl+C or
// SIGTERM), and an error if the config is bad, an upstream fails to start, or
// an upstream exits while the client is connected.
//
// getenv reads environment variables (os.Getenv in production), so tests can
// supply a key without changing the real environment.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) > 0 && args[0] == "keygen" {
		return runKeygen(args[1:], stdout, stderr)
	}
	f, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: f.logLevel}))

	authn, err := auth.New(agentEntries(cfg))
	if err != nil {
		return err
	}
	tlsConfig, err := loadTLS(f)
	if err != nil {
		return err
	}
	if f.httpAddr == "" {
		// A stdio gateway serves exactly one client, so it checks that
		// client's key once, before starting any upstream.
		agent, err := authn.Authenticate(getenv(apiKeyEnv))
		if err != nil {
			return fmt.Errorf("%s: %w", apiKeyEnv, err)
		}
		logger.Info("agent authenticated", "agent", agent.Name)
		ctx = auth.WithAgent(ctx, agent)
	}

	// Upstream read loops start inside StartGroup, but the proxy can only be
	// built after it. Notifications that arrive before the proxy exists are
	// dropped, the same as when no client is connected.
	// Progress goes back to the request that asked for it. Other upstream
	// notifications go to the stdio client, if there is one.
	tracker := progress.NewTracker()
	var prx atomic.Pointer[proxy.Proxy]
	forward := func(msg *jsonrpc.Message) {
		if tracker.Deliver(msg) {
			return
		}
		if p := prx.Load(); p != nil {
			p.ForwardNotification(msg)
			return
		}
		logger.Debug("dropping upstream notification before proxy start", "method", msg.Method)
	}

	group, err := upstream.StartGroup(ctx, upstreamSpecs(cfg), upstream.Options{
		OnNotification: forward,
		Logger:         logger.With("component", "upstream"),
		Stderr:         stderr,
	})
	if err != nil {
		return err
	}
	for _, u := range cfg.Upstreams {
		// Only the command is logged. Arguments can hold tokens.
		logger.Info("upstream started", "upstream", u.Name, "command", u.Command)
	}

	rt, err := router.New(routes(group), router.Options{Logger: logger.With("component", "router")})
	if err != nil {
		return errors.Join(err, group.Close())
	}
	p := proxy.New(rt, proxy.Options{
		CallTimeout: f.callTimeout,
		Logger:      logger.With("component", "proxy"),
		Progress:    tracker,
	})
	prx.Store(p)

	var serveErr error
	if f.httpAddr != "" {
		serveErr = serveHTTP(ctx, rt, authn, tracker, group, f, tlsConfig, logger)
	} else {
		serveErr = serve(ctx, p, group, stdin, stdout)
	}
	if ctx.Err() != nil {
		// Ctrl+C or SIGTERM is a normal way to stop.
		logger.Info("shutting down on signal")
		serveErr = nil
	}

	closeErr := group.Close()
	if closeErr != nil {
		logger.Warn("upstream shutdown", "error", closeErr)
	}
	logger.Info("gateway stopped")
	return errors.Join(serveErr, closeErr)
}

func agentEntries(cfg *config.Config) []auth.Entry {
	entries := make([]auth.Entry, len(cfg.Agents))
	for i, a := range cfg.Agents {
		entries[i] = auth.Entry{Name: a.Name, KeySHA256: a.KeySHA256}
	}
	return entries
}

func upstreamSpecs(cfg *config.Config) []upstream.Spec {
	specs := make([]upstream.Spec, len(cfg.Upstreams))
	for i, u := range cfg.Upstreams {
		specs[i] = upstream.Spec{Name: u.Name, Command: upstream.Command{Path: u.Command, Args: u.Args}}
	}
	return specs
}

func routes(g *upstream.Group) []router.Named {
	members := g.Members()
	named := make([]router.Named, len(members))
	for i, m := range members {
		named[i] = router.Named{Name: m.Name, Upstream: m.Process}
	}
	return named
}

// serve runs the proxy until the client is done, ctx is cancelled, or an
// upstream exits. Without the upstream watch, a dead upstream would leave the
// client connected to a gateway that can no longer reach some of its tools.
func serve(ctx context.Context, p *proxy.Proxy, group *upstream.Group, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		select {
		case <-group.Done():
			cancel(fmt.Errorf("%w: %w", errUpstreamExited, group.Err()))
		case <-ctx.Done():
		}
	}()

	return p.Serve(ctx, stdin, stdout)
}
