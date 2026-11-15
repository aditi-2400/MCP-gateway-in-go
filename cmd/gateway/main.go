// Command gateway is an MCP gateway. It speaks MCP to one client over its own
// stdin and stdout, and routes requests to the upstream MCP servers listed in
// its config file, each started as a child process.
//
// Usage:
//
//	gateway -config gateway.yaml -policy policy.yaml [-call-timeout 60s] [-log-level info]
//	gateway -config gateway.yaml -policy policy.yaml -http 127.0.0.1:8080 [-allow-origin URL]...
//	gateway ... -metrics 127.0.0.1:9090
//	gateway ... -trace     (collector from OTEL_EXPORTER_OTLP_ENDPOINT)
//	gateway keygen -agent NAME
//	gateway audit -db audit.db [-agent NAME] [-tool NAME] [-decision D] [-since 1h] [-json]
//
// Every caller needs an API key from keygen. Over stdio it goes in the
// MCP_GATEWAY_API_KEY environment variable; over HTTP, in an
// "Authorization: Bearer" header on every request.
//
// Without -http it speaks MCP over stdin and stdout, and logs go to stderr
// because stdout carries MCP messages. With -http it serves Streamable HTTP
// at /mcp and does not read stdin.
//
// With -metrics it also serves Prometheus metrics at /metrics on a separate
// address, in either mode.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/config"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/metrics"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/policy"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/progress"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/proxy"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/ratelimit"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/router"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/tracing"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

// errUsage means the command line was wrong. The message and usage text
// have already been printed.
var errUsage = errors.New("usage error")

// errUpstreamExited means an upstream server stopped while the client was
// still connected.
var errUpstreamExited = errors.New("upstream server exited")

// auditFlushTimeout bounds how long shutdown waits for audit entries to be
// written.
const auditFlushTimeout = 10 * time.Second

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
	configPath         string
	policyPath         string
	auditDB            string
	auditQueue         int
	auditWait          time.Duration
	callTimeout        time.Duration
	logLevel           slog.Level
	httpAddr           string // empty means stdio
	allowedOrigins     stringList
	tlsCert            string
	tlsKey             string
	behindTLSProxy     bool
	metricsAddr        string // empty means no metrics
	metricsAllowRemote bool
	trace              bool
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
	fs.StringVar(&f.policyPath, "policy", "", "path to the YAML policy file saying which agent may call which tool (required)")
	fs.StringVar(&f.auditDB, "audit-db", "", "path to the SQLite audit log, created if missing (required)")
	fs.IntVar(&f.auditQueue, "audit-queue", audit.DefaultQueueSize, "most tool calls in flight plus audit entries not yet written")
	fs.DurationVar(&f.auditWait, "audit-wait", audit.DefaultReserveWait, "how long a tool call waits for room in the audit queue before it is refused")
	fs.DurationVar(&f.callTimeout, "call-timeout", proxy.DefaultCallTimeout, "how long each request may wait for the upstreams")
	fs.TextVar(&f.logLevel, "log-level", f.logLevel, "log level: debug, info, warn, or error")
	fs.StringVar(&f.httpAddr, "http", "", "serve Streamable HTTP on this address (e.g. 127.0.0.1:8080) in place of stdio; non-loopback addresses need TLS or -behind-tls-proxy")
	fs.Var(&f.allowedOrigins, "allow-origin", "browser Origin allowed to call the HTTP endpoint (repeatable)")
	fs.StringVar(&f.tlsCert, "tls-cert", "", "PEM certificate file; serve HTTPS (needs -tls-key)")
	fs.StringVar(&f.tlsKey, "tls-key", "", "PEM private key file for -tls-cert")
	fs.BoolVar(&f.behindTLSProxy, "behind-tls-proxy", false, "allow plain HTTP on a non-loopback address because a proxy in front terminates TLS")
	fs.StringVar(&f.metricsAddr, "metrics", "", "serve Prometheus metrics at /metrics on this address (e.g. 127.0.0.1:9090); loopback only unless -metrics-allow-remote")
	fs.BoolVar(&f.metricsAllowRemote, "metrics-allow-remote", false, "allow -metrics on a non-loopback address; the endpoint has no authentication")
	fs.BoolVar(&f.trace, "trace", false, "send OpenTelemetry traces over OTLP/HTTP; the collector address comes from OTEL_EXPORTER_OTLP_ENDPOINT (default https://localhost:4318)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags{}, err
		}
		return flags{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.configPath == "" {
		return flags{}, usageFailure(fs, "missing -config")
	}
	if f.policyPath == "" {
		return flags{}, usageFailure(fs, "missing -policy; every call is checked against a policy file")
	}
	if f.auditDB == "" {
		return flags{}, usageFailure(fs, "missing -audit-db; every tool call is recorded")
	}
	if f.auditQueue <= 0 {
		return flags{}, usageFailure(fs, "-audit-queue must be positive")
	}
	if f.auditWait <= 0 {
		return flags{}, usageFailure(fs, "-audit-wait must be positive")
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
	if err := checkMetricsFlags(f); err != nil {
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
	if len(args) > 0 && args[0] == "audit" {
		return runAudit(ctx, args[1:], stdout, stderr)
	}
	f, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	pol, err := loadPolicy(f, cfg)
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

	// Listening now means a busy port stops startup before any upstream
	// starts. Serving waits until the metrics exist, below.
	var metricsLn net.Listener
	if f.metricsAddr != "" {
		metricsLn, err = net.Listen("tcp", f.metricsAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", f.metricsAddr, err)
		}
		// Closes the listener if startup fails before the metrics server
		// takes it over. Once it has, the server closes it and this second
		// close only reports that it is already closed.
		defer metricsLn.Close()
	}

	// Opened before upstreams start, so a bad path stops startup early.
	store, err := audit.OpenSQLite(ctx, f.auditDB)
	if err != nil {
		return err
	}
	recorder := audit.NewRecorder(store, audit.Options{
		QueueSize:   f.auditQueue,
		ReserveWait: f.auditWait,
		Logger:      logger.With("component", "audit"),
	})
	closeAudit := func() error {
		flushCtx, cancel := context.WithTimeout(context.Background(), auditFlushTimeout)
		defer cancel()
		return errors.Join(recorder.Close(flushCtx), store.Close())
	}

	tracer, stopTracing, err := startTracing(f.trace, logger.With("component", "tracing"))
	if err != nil {
		return errors.Join(err, closeAudit())
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
		stopTracing()
		return errors.Join(err, closeAudit())
	}
	for _, u := range cfg.Upstreams {
		// Only the command is logged. Arguments can hold tokens.
		logger.Info("upstream started", "upstream", u.Name, "command", u.Command)
	}

	// Every upstream call goes through tracing.WrapUpstream, even with
	// tracing off: it removes the trace keys a client put in _meta, which are
	// untrusted, before anything reaches an upstream. Span names need the
	// router's tool list, and the router is built from the wrapped upstreams,
	// so knownTool reads rt once it is set; no call runs before that.
	var rt *router.Router
	knownTool := func(name string) bool { return rt.KnownTool(name) }
	rt, err = router.New(routes(group, tracer, knownTool), router.Options{Logger: logger.With("component", "router")})
	if err != nil {
		stopTracing()
		return errors.Join(err, closeAudit(), group.Close())
	}
	// A request passes audit, policy, rate limit, the upstream timer, then the
	// router. Audit comes first so even refused calls are recorded, and a
	// call that can't be recorded is refused before anything else. The
	// policy runs before the rate limit, so denied calls use up no tokens.
	timed := audit.TimeUpstream(rt)
	limited := ratelimit.NewEnforcer(timed, pol, ratelimit.New(time.Now), logger.With("component", "ratelimit"))
	guard := policy.NewGuard(limited, rt, pol, logger.With("component", "policy"))
	audited := audit.NewLayer(guard, recorder, logger.With("component", "audit"))
	// With -trace, a span covers each request from before the audit layer, so
	// it includes any wait for room in the audit queue.
	var handler proxy.Handler = audited
	if f.trace {
		handler = tracing.NewLayer(audited, tracer, rt.KnownTool)
	}
	p := proxy.New(handler, proxy.Options{
		CallTimeout: f.callTimeout,
		Logger:      logger.With("component", "proxy"),
		Progress:    tracker,
	})
	prx.Store(p)

	var ms *metricsServer
	if metricsLn != nil {
		m := metrics.New(rt.KnownTool, knownAgents(cfg))
		m.WatchAuditQueue(recorder.Used, recorder.Capacity)
		audited.SetObserver(m)
		ms = startMetrics(metricsLn, m.Handler(), logger.With("component", "metrics"))
	}

	var serveErr error
	if f.httpAddr != "" {
		serveErr = serveHTTP(ctx, handler, guard, authn, tracker, group, f, tlsConfig, logger)
	} else {
		serveErr = serve(ctx, p, group, stdin, stdout)
	}
	if ctx.Err() != nil {
		// Ctrl+C or SIGTERM is a normal way to stop.
		logger.Info("shutting down on signal")
		serveErr = nil
	}

	var metricsErr error
	if ms != nil {
		if metricsErr = ms.stop(); metricsErr != nil {
			logger.Warn("metrics shutdown", "error", metricsErr)
		}
	}
	// Every span has ended, so send what is still buffered.
	stopTracing()

	// Every call has finished, so flush the audit log before stopping
	// upstreams, in case stopping them hangs.
	auditErr := closeAudit()
	if auditErr != nil {
		logger.Error("audit log shutdown", "error", auditErr)
	}
	closeErr := group.Close()
	if closeErr != nil {
		logger.Warn("upstream shutdown", "error", closeErr)
	}
	logger.Info("gateway stopped")
	return errors.Join(serveErr, metricsErr, auditErr, closeErr)
}

// loadPolicy reads the policy and checks it against the config, so a rule
// for a misspelled upstream or an agent with no key stops startup.
func loadPolicy(f flags, cfg *config.Config) (*policy.Policy, error) {
	pol, err := policy.Load(f.policyPath)
	if err != nil {
		return nil, err
	}
	upstreams := make([]string, len(cfg.Upstreams))
	for i, u := range cfg.Upstreams {
		upstreams[i] = u.Name
	}
	agents := make([]string, len(cfg.Agents))
	for i, a := range cfg.Agents {
		agents[i] = a.Name
	}
	if err := pol.CheckNames(upstreams, agents); err != nil {
		return nil, fmt.Errorf("policy %s does not match config %s: %w", f.policyPath, f.configPath, err)
	}
	return pol, nil
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

// routes names each upstream for the router, wrapped for tracing.
func routes(g *upstream.Group, tracer trace.Tracer, knownTool func(string) bool) []router.Named {
	members := g.Members()
	named := make([]router.Named, len(members))
	for i, m := range members {
		named[i] = router.Named{Name: m.Name, Upstream: tracing.WrapUpstream(m.Name, m.Process, tracer, knownTool)}
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
