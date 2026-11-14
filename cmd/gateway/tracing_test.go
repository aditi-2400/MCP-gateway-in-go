package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// clientTraceMeta is _meta with trace keys a client chose.
const clientTraceMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},` +
	`"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","tracestate":"vendor=x","baggage":"user=alice"}`

// The fake upstream's meta tool returns the trace keys it received.
func TestClientTraceKeysNeverReachTheUpstream(t *testing.T) {
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"))
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__meta",` + clientTraceMeta + `}}`)
	resp := g.next()
	if resp.Error != nil {
		t.Fatalf("tools/call error = %v", resp.Error)
	}
	var result struct {
		StructuredContent map[string]string `json:"structuredContent"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	for k, v := range result.StructuredContent {
		if strings.Contains(v, "4bf92f3577b34da6a3ce929d0e0e4736") || strings.Contains(v, "alice") || strings.Contains(v, "vendor") {
			t.Errorf("upstream got the client's %s: %q", k, v)
		}
	}
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\n%s", err, g.stderr)
	}
}
