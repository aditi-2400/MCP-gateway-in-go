package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// An upstream's own timeout ends its slow calls early, and leaves other
// upstreams alone.
func TestUpstreamTimeoutFromConfig(t *testing.T) {
	cfg := writeConfig(t,
		upstreamEntry{name: "slow", command: fakeUpstreamBin, timeout: "100ms"},
		upstreamEntry{name: "fast", command: fakeUpstreamBin},
	)
	g := startGateway(t, context.Background(), "-config", cfg)

	start := time.Now()
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow__sleep","arguments":{"ms":1000},` + validMeta + `}}`)
	resp := g.next()
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError || resp.Error.Message != "upstream timed out" {
		t.Fatalf("slow call: result %s, error %v; want upstream timed out", resp.Result, resp.Error)
	}
	if took := time.Since(start); took >= time.Second {
		t.Errorf("slow call took %v, want it to stop at the 100ms timeout", took)
	}

	// No timeout of its own, so only -call-timeout (60s) applies.
	g.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast__sleep","arguments":{"ms":300},` + validMeta + `}}`)
	if resp := g.next(); resp.Error != nil || !strings.Contains(string(resp.Result), "slept 300ms") {
		t.Fatalf("fast call: result %s, error %v; want slept 300ms", resp.Result, resp.Error)
	}

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\nstderr:\n%s", err, g.stderr)
	}
	if !strings.Contains(g.stderr.String(), `msg="upstream started" upstream=slow command=`) || !strings.Contains(g.stderr.String(), "timeout=100ms") {
		t.Errorf("stderr does not log slow's timeout:\n%s", g.stderr)
	}
}
