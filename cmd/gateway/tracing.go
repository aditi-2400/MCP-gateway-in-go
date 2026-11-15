package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	// serviceName names the gateway in trace viewers. OTEL_SERVICE_NAME
	// overrides it.
	serviceName = "mcp-gateway"
	// tracerName identifies the code that made the spans.
	tracerName = "github.com/aditi-2400/MCP-gateway-in-go"
	// traceFlushTimeout bounds how long shutdown waits to send the spans
	// still buffered. A collector that is down shouldn't hold up exit.
	traceFlushTimeout = 5 * time.Second
)

// startTracing returns the tracer to use and a function that sends any
// buffered spans and stops tracing. Without -trace the tracer records
// nothing and stop does nothing.
//
// With -trace, spans go out over OTLP/HTTP in batches. Where they go, and
// how many are kept, come from the standard OpenTelemetry environment
// variables (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_SAMPLER, ...), read by
// the SDK itself.
func startTracing(enabled bool, logger *slog.Logger) (trace.Tracer, func(), error) {
	if !enabled {
		return noop.NewTracerProvider().Tracer(""), func() {}, nil
	}
	// The SDK reports export failures here. Without this they would go to
	// the standard log package with no level or component.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("trace export failed", "error", err)
	}))

	// Not the run context: spans buffered when Ctrl+C arrives still have to
	// be sent by stop.
	ctx := context.Background()
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("start trace exporter: %w", err)
	}
	// Later options win, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
	// override the default name.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("trace resource: %w", err), exporter.Shutdown(ctx))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	logger.Info("tracing on", "exporter", "otlp/http")

	stop := func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), traceFlushTimeout)
		defer cancel()
		// Losing spans is logged; it doesn't make the gateway's exit fail.
		if err := tp.Shutdown(flushCtx); err != nil {
			logger.Warn("trace flush on shutdown", "error", err)
		}
	}
	return tp.Tracer(tracerName), stop, nil
}
