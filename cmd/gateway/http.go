package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/httpfront"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/progress"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/router"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

const (
	// readHeaderTimeout stops clients that open a connection and send headers
	// slowly, holding a connection open for free.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds reading a whole request, body included.
	readTimeout = 30 * time.Second
	// idleTimeout closes keep-alive connections nobody is using.
	idleTimeout = 60 * time.Second
	// httpShutdownTimeout is how long in-flight requests get to finish after
	// Ctrl+C or SIGTERM before their connections are closed.
	httpShutdownTimeout = 10 * time.Second
)

// checkLoopback refuses any listen address that is not loopback. Until the
// gateway has authentication, listening on a network address would let
// anyone who can reach it call every upstream tool.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-http %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	// An empty host (":8080") means every interface.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-http %q: only loopback addresses (127.0.0.1, ::1, localhost) are allowed until auth exists", addr)
}

// serveHTTP serves Streamable HTTP until ctx is cancelled or an upstream
// exits, then lets in-flight requests finish before returning.
func serveHTTP(ctx context.Context, rt *router.Router, tracker *progress.Tracker, group *upstream.Group, f flags, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", f.httpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.httpAddr, err)
	}
	srv := &http.Server{
		Handler: httpfront.New(rt, httpfront.Options{
			AllowedOrigins: f.allowedOrigins,
			CallTimeout:    f.callTimeout,
			Logger:         logger.With("component", "http"),
			Progress:       tracker,
			Schemas:        rt,
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// The real address matters when the port is 0 (tests pick a free port).
	logger.Info("http listening", "addr", ln.Addr().String(), "path", httpfront.Path)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var stopErr error
	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	case <-group.Done():
		stopErr = fmt.Errorf("%w: %w", errUpstreamExited, group.Err())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "error", err)
		srv.Close()
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		stopErr = errors.Join(stopErr, fmt.Errorf("http server: %w", err))
	}
	return stopErr
}
