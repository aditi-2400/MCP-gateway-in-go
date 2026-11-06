// Command gateway is an MCP gateway. It speaks MCP to one client over its own
// stdin and stdout, and forwards every message to one upstream MCP server
// that it starts as a child process.
//
// Usage:
//
//	gateway [flags] -- upstream-command [args...]
//
// Logs go to stderr, because stdout carries MCP messages.
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/proxy"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

// errUsage means the command line was wrong. The message and usage text
// have already been printed.
var errUsage = errors.New("usage error")

// errUpstreamExited means the upstream server stopped while the client was
// still connected.
var errUpstreamExited = errors.New("upstream server exited")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
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

type config struct {
	upstream    upstream.Command
	callTimeout time.Duration
	logLevel    slog.Level
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gateway [flags] -- upstream-command [args...]")
		fs.PrintDefaults()
	}

	cfg := config{logLevel: slog.LevelInfo}
	fs.DurationVar(&cfg.callTimeout, "call-timeout", proxy.DefaultCallTimeout, "how long each request may wait for the upstream")
	fs.TextVar(&cfg.logLevel, "log-level", cfg.logLevel, "log level: debug, info, warn, or error")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config{}, err
		}
		return config{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if cfg.callTimeout <= 0 {
		return config{}, usageFailure(fs, "-call-timeout must be positive")
	}
	if fs.NArg() == 0 {
		return config{}, usageFailure(fs, "missing upstream command")
	}

	cfg.upstream = upstream.Command{Path: fs.Arg(0), Args: fs.Args()[1:]}
	return cfg, nil
}

func usageFailure(fs *flag.FlagSet, msg string) error {
	fmt.Fprintln(fs.Output(), "gateway:", msg)
	fs.Usage()
	return fmt.Errorf("%w: %s", errUsage, msg)
}

// run is the whole program, minus process exit codes, so tests can drive it.
// It returns nil when the client closes stdin or ctx is cancelled (Ctrl+C or
// SIGTERM), and an error if the upstream exits while the client is connected.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.logLevel}))

	// The upstream read loop starts inside upstream.Start, but the proxy can
	// only be built after it. Notifications that arrive before the proxy
	// exists are dropped, the same as when no client is connected.
	var prx atomic.Pointer[proxy.Proxy]
	forward := func(msg *jsonrpc.Message) {
		if p := prx.Load(); p != nil {
			p.ForwardNotification(msg)
			return
		}
		logger.Debug("dropping upstream notification before proxy start", "method", msg.Method)
	}

	proc, err := upstream.Start(cfg.upstream, upstream.Options{
		OnNotification: forward,
		Logger:         logger.With("component", "upstream"),
		Stderr:         stderr,
	})
	if err != nil {
		return err
	}
	// Only the path is logged. Arguments can hold tokens.
	logger.Info("upstream started", "path", cfg.upstream.Path)

	p := proxy.New(proc, proxy.Options{
		CallTimeout: cfg.callTimeout,
		Logger:      logger.With("component", "proxy"),
	})
	prx.Store(p)

	serveErr := serve(ctx, p, proc, stdin, stdout)
	if ctx.Err() != nil {
		// Ctrl+C or SIGTERM is a normal way to stop.
		logger.Info("shutting down on signal")
		serveErr = nil
	}

	closeErr := proc.Close()
	if closeErr != nil {
		logger.Warn("upstream shutdown", "error", closeErr)
	}
	logger.Info("gateway stopped")
	return errors.Join(serveErr, closeErr)
}

// serve runs the proxy until the client is done, ctx is cancelled, or the
// upstream exits. Without the upstream watch, a dead upstream would leave the
// client connected to a gateway that answers every request with an error.
func serve(ctx context.Context, p *proxy.Proxy, proc *upstream.Process, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		select {
		case <-proc.Done():
			cancel(fmt.Errorf("%w: %w", errUpstreamExited, proc.Err()))
		case <-ctx.Done():
		}
	}()

	return p.Serve(ctx, stdin, stdout)
}
