package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkRecorderSQLite measures how many entries a second the audit log
// can save, with many calls committing at once, as under load. Close is
// timed too: an entry only counts once it is on disk.
//
//	go test ./internal/audit -run '^$' -bench RecorderSQLite -cpu 8
func BenchmarkRecorderSQLite(b *testing.B) {
	store, err := OpenSQLite(context.Background(), filepath.Join(b.TempDir(), "audit.db"))
	if err != nil {
		b.Fatal(err)
	}
	// A long wait: the benchmark measures how fast slots free up, so calls
	// should wait for them, not be refused.
	rec := NewRecorder(store, Options{ReserveWait: time.Minute})
	entry := Entry{
		Agent: "bench", Tool: "fake__add", Arguments: json.RawMessage(`{"a":1,"b":2}`),
		Decision: Allowed, Reason: `role "all" allows fake__add`, Rule: "all: fake__*",
		Status: StatusOK, Latency: 20 * time.Microsecond,
	}

	b.SetParallelism(4) // 4 goroutines per CPU: 32 with -cpu 8, like the gateway runs
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot, err := rec.Reserve(context.Background())
			if err != nil {
				b.Error(err)
				return
			}
			e := entry
			e.Time = time.Now()
			e.RequestID = rand.Text()
			slot.Commit(e)
		}
	})
	if err := rec.Close(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "entries/s")
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
}
