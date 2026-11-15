package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/loadtest"
)

// benchClients is how many HTTP clients call at once. It is past the point
// where calls per second stop growing (see PROGRESS, step 6c).
const benchClients = 32

// BenchmarkHTTPThroughput runs the gateway in this process over HTTP, in
// front of the fake upstream, and keeps benchClients clients calling it.
// It exists so Go's profilers can see inside the gateway under load:
//
//	go test ./cmd/gateway -run '^$' -bench HTTPThroughput -benchtime 1x \
//	    -cpuprofile cpu.out -blockprofile block.out -mutexprofile mutex.out -o gateway.test
//
// The load clients run in this process too, so their work shows up in the
// profile under internal/loadtest and net/http's client.
func BenchmarkHTTPThroughput(b *testing.B) {
	key, err := auth.GenerateKey()
	if err != nil {
		b.Fatal(err)
	}
	args, err := loadtest.WriteGatewayFiles(b.TempDir(), fakeUpstreamBin, auth.HashKey(key))
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, append(args, "-http", "127.0.0.1:0"), strings.NewReader(""), io.Discard, stderr, testEnv)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			b.Errorf("run: %v\n%s", err, stderr)
		}
	}()

	url := ""
	for deadline := time.Now().Add(testTimeout); url == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if m := listenLine.FindStringSubmatch(stderr.String()); m != nil {
			url = "http://" + m[1] + "/mcp"
		}
	}
	if url == "" {
		b.Fatalf("gateway did not start listening\n%s", stderr)
	}

	target := loadtest.HTTPTarget{URL: url, Key: key, Tool: loadtest.UpstreamName + "__add"}
	client := loadtest.NewHTTPClient(benchClients)
	defer client.CloseIdleConnections()
	for b.Loop() {
		result, err := loadtest.Concurrent(ctx, client, target, benchClients, time.Second/2, 3*time.Second)
		if err != nil {
			b.Fatalf("%v\n%s", err, stderr)
		}
		b.ReportMetric(result.PerSecond, "calls/s")
		b.ReportMetric(float64(result.Latency.P99.Microseconds()), "p99_µs")
	}
}
