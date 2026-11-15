package upstream

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// BenchmarkConnCallsInParallel measures calls a second through one
// connection to the fake upstream, with many callers at once, as when the
// gateway forwards many agents' calls to one stdio server.
//
//	go test ./internal/upstream -run '^$' -bench ConnCallsInParallel -cpu 8
func BenchmarkConnCallsInParallel(b *testing.B) {
	p, err := Start(Command{Path: fakeUpstreamBin}, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			b.Error(err)
		}
	}()
	params := json.RawMessage(`{"name":"add","arguments":{"a":1,"b":2},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`)

	b.SetParallelism(4) // 32 callers with -cpu 8, like the gateway runs
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := p.Call(context.Background(), "tools/call", params)
			if err != nil || resp.Error != nil {
				b.Errorf("call: %v %v", err, resp)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "calls/s")
}
