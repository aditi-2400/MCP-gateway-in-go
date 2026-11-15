package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otlpReceiver is a fake trace collector: it accepts OTLP/HTTP protobuf
// exports and keeps every span.
type otlpReceiver struct {
	mu       sync.Mutex
	requests int
	spans    []*tracepb.Span
	services []string
}

// startReceiver starts a receiver and points the exporter at it.
func startReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	recv := &otlpReceiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/v1/traces" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req collectorpb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		recv.mu.Lock()
		defer recv.mu.Unlock()
		recv.requests++
		for _, rs := range req.ResourceSpans {
			for _, kv := range rs.GetResource().GetAttributes() {
				if kv.Key == "service.name" {
					recv.services = append(recv.services, kv.GetValue().GetStringValue())
				}
			}
			for _, ss := range rs.ScopeSpans {
				recv.spans = append(recv.spans, ss.Spans...)
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	return recv
}

func (r *otlpReceiver) span(t *testing.T, name string, kind tracepb.Span_SpanKind) *tracepb.Span {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.spans {
		if s.Name == name && s.Kind == kind {
			return s
		}
	}
	names := make([]string, len(r.spans))
	for i, s := range r.spans {
		names[i] = s.Name + " (" + s.Kind.String() + ")"
	}
	t.Fatalf("no %s span %q; got %v", kind, name, names)
	return nil
}

func TestRunExportsTracesWithTrace(t *testing.T) {
	recv := startReceiver(t)
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"), "-trace")
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()
	g.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fake__meta",` + clientTraceMeta + `}}`)
	resp := g.next()
	var result struct {
		StructuredContent struct {
			Traceparent string `json:"traceparent"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	// Spans are sent in batches; the last batch goes out on shutdown.
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\n%s", err, g.stderr)
	}

	recv.span(t, "tools/list", tracepb.Span_SPAN_KIND_SERVER)
	server := recv.span(t, "tools/call fake__meta", tracepb.Span_SPAN_KIND_SERVER)
	client := recv.span(t, "tools/call meta", tracepb.Span_SPAN_KIND_CLIENT)

	if string(client.TraceId) != string(server.TraceId) || string(client.ParentSpanId) != string(server.SpanId) {
		t.Error("the upstream call's span is not a child of the request's span")
	}
	want := "00-" + hex.EncodeToString(client.TraceId) + "-" + hex.EncodeToString(client.SpanId) + "-01"
	if result.StructuredContent.Traceparent != want {
		t.Errorf("upstream got traceparent %q, want the gateway's %q", result.StructuredContent.Traceparent, want)
	}
	if hex.EncodeToString(server.TraceId) == "4bf92f3577b34da6a3ce929d0e0e4736" || len(server.ParentSpanId) != 0 {
		t.Error("the request's span joined the client's trace")
	}
	if len(server.Links) != 1 || hex.EncodeToString(server.Links[0].TraceId) != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("request span links = %v, want one link to the client's trace", server.Links)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.services) == 0 {
		t.Error("spans came with no service.name")
	}
	for _, s := range recv.services {
		if s != serviceName {
			t.Errorf("service.name = %q, want %q", s, serviceName)
		}
	}
}

func TestRunSendsNoTracesWithoutTrace(t *testing.T) {
	recv := startReceiver(t)
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"))
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__add","arguments":{"a":1,"b":2},` + validMeta + `}}`)
	g.next()
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if recv.requests != 0 {
		t.Errorf("collector got %d exports without -trace", recv.requests)
	}
}

// A collector that is down costs at most traceFlushTimeout at shutdown and
// doesn't fail the exit.
func TestRunStopsWhenTheCollectorIsDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"), "-trace")
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__add","arguments":{"a":1,"b":2},` + validMeta + `}}`)
	if resp := g.next(); resp.Error != nil {
		t.Fatalf("tools/call error = %v; a missing collector must not affect calls", resp.Error)
	}
	start := time.Now()
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if took := time.Since(start); took > traceFlushTimeout+3*time.Second {
		t.Errorf("shutdown took %v with the collector down, want about %v at most", took, traceFlushTimeout)
	}
	// The batcher reports the failed send through the error handler.
	if !strings.Contains(g.stderr.String(), `msg="trace export failed"`) {
		t.Errorf("no warning about lost spans:\n%s", g.stderr)
	}
}

func TestOTELServiceNameOverridesTheDefault(t *testing.T) {
	recv := startReceiver(t)
	t.Setenv("OTEL_SERVICE_NAME", "gateway-staging")
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"), "-trace")
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.services) == 0 || recv.services[0] != "gateway-staging" {
		t.Errorf("service.name = %v, want gateway-staging", recv.services)
	}
}
