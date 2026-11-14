package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
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

// checkHTTPFlags enforces the transport rules. A non-loopback address is
// only allowed with TLS, or when the operator says a proxy in front handles
// TLS: over plain HTTP on a network, anyone on the path could read the API
// keys in the Authorization headers.
func checkHTTPFlags(f flags) error {
	if f.httpAddr == "" {
		switch {
		case len(f.allowedOrigins) > 0:
			return errors.New("-allow-origin needs -http")
		case f.tlsCert != "" || f.tlsKey != "":
			return errors.New("-tls-cert and -tls-key need -http")
		case f.behindTLSProxy:
			return errors.New("-behind-tls-proxy needs -http")
		}
		return nil
	}
	if (f.tlsCert == "") != (f.tlsKey == "") {
		return errors.New("-tls-cert and -tls-key must be given together")
	}
	useTLS := f.tlsCert != ""
	if useTLS && f.behindTLSProxy {
		return errors.New("use either -tls-cert/-tls-key or -behind-tls-proxy, not both")
	}

	loopback, err := isLoopback(f.httpAddr)
	if err != nil {
		return err
	}
	if !loopback && !useTLS && !f.behindTLSProxy {
		return fmt.Errorf("-http %q: a non-loopback address needs -tls-cert and -tls-key, or -behind-tls-proxy if a proxy in front terminates TLS; plain HTTP would expose API keys on the network", f.httpAddr)
	}
	return nil
}

// isLoopback reports whether addr only accepts connections from this
// machine. An empty host (":8080") means every interface.
func isLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("-http %q: %w", addr, err)
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

// loadTLS reads the certificate, if any. run calls it before starting
// upstreams, so a bad certificate stops startup before any child process
// starts, and not on the first connection.
func loadTLS(f flags) (*tls.Config, error) {
	if f.tlsCert == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(f.tlsCert, f.tlsKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// serveHTTP serves on f.httpAddr, over TLS if tlsConfig is set.
func serveHTTP(ctx context.Context, rt *router.Router, authn *auth.Authenticator, tracker *progress.Tracker, group *upstream.Group, f flags, tlsConfig *tls.Config, logger *slog.Logger) error {

	ln, err := net.Listen("tcp", f.httpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.httpAddr, err)
	}
	srv := &http.Server{
		TLSConfig: tlsConfig,
		Handler: httpfront.New(rt, httpfront.Options{
			Auth:           authn,
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
	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}
	logger.Info("http listening", "addr", ln.Addr().String(), "scheme", scheme, "path", httpfront.Path)
	if f.behindTLSProxy {
		logger.Warn("serving plain HTTP; a proxy in front must terminate TLS, or API keys cross the network unencrypted")
	}

	serveErr := make(chan error, 1)
	go func() {
		if tlsConfig != nil {
			// The certificate is already in TLSConfig, so no file names here.
			serveErr <- srv.ServeTLS(ln, "", "")
			return
		}
		serveErr <- srv.Serve(ln)
	}()

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
