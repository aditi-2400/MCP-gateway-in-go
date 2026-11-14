package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/config"
)

// metricsPath is where the metrics listener serves.
const metricsPath = "/metrics"

// metricsWriteTimeout bounds writing one scrape. The handler gives up after
// 10 seconds on its own, so this only catches a scraper that stops reading.
const metricsWriteTimeout = 30 * time.Second

// checkMetricsFlags keeps metrics on loopback unless the operator opts in.
// The endpoint has no authentication, and it shows agent and tool names and
// how busy each one is.
func checkMetricsFlags(f flags) error {
	if f.metricsAddr == "" {
		if f.metricsAllowRemote {
			return errors.New("-metrics-allow-remote needs -metrics")
		}
		return nil
	}
	loopback, err := isLoopback(f.metricsAddr)
	if err != nil {
		return fmt.Errorf("-metrics %w", err)
	}
	if !loopback && !f.metricsAllowRemote {
		return fmt.Errorf("-metrics %q: a non-loopback address needs -metrics-allow-remote; metrics have no authentication and show agent and tool names", f.metricsAddr)
	}
	return nil
}

// knownAgents reports whether a name is an agent in the config.
func knownAgents(cfg *config.Config) func(string) bool {
	names := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		names[a.Name] = true
	}
	return func(name string) bool { return names[name] }
}

// metricsServer serves the metrics on their own listener, apart from MCP
// traffic, so a scraper never needs an API key and agents never see metrics.
type metricsServer struct {
	srv  *http.Server
	done chan error
}

// startMetrics serves h at /metrics on ln until stop is called. If the
// server fails on its own, it logs the error and the gateway keeps serving
// tool calls: losing metrics shouldn't take tools down, and Prometheus
// notices a target it can't scrape.
func startMetrics(ln net.Listener, h http.Handler, logger *slog.Logger) *metricsServer {
	mux := http.NewServeMux()
	mux.Handle("GET "+metricsPath, h)
	ms := &metricsServer{
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      metricsWriteTimeout,
			IdleTimeout:       idleTimeout,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		done: make(chan error, 1),
	}
	logger.Info("metrics listening", "addr", ln.Addr().String(), "path", metricsPath)
	go func() {
		err := ms.srv.Serve(ln)
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped", "error", err)
		}
		ms.done <- err
	}()
	return ms
}

// stop lets running scrapes finish, then closes the listener.
func (ms *metricsServer) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	var err error
	if shutdownErr := ms.srv.Shutdown(ctx); shutdownErr != nil {
		err = errors.Join(fmt.Errorf("metrics shutdown: %w", shutdownErr), ms.srv.Close())
	}
	if serveErr := <-ms.done; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, fmt.Errorf("metrics server: %w", serveErr))
	}
	return err
}
