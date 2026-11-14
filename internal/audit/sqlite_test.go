package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	s, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func sampleEntry(id string) Entry {
	return Entry{
		Time:      time.Date(2026, 11, 14, 10, 2, 59, 33_000_000, time.UTC),
		RequestID: id,
		Agent:     "ci-bot",
		Tool:      "fs__write_file",
		Arguments: json.RawMessage(`{"path":"/tmp/x","token":"[REDACTED]"}`),
		Decision:  Allowed,
		Reason:    `role "writer" allows fs__write_file`,
		Rule:      "writer: fs__write_file",
		Status:    StatusOK,
		Latency:   1500 * time.Microsecond,
	}
}

func count(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM audit_log").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSQLiteRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	limited := sampleEntry("r2")
	limited.Decision, limited.Status, limited.ErrorCode, limited.Latency = RateLimited, StatusRPCError, -31029, 0
	if err := s.Write(context.Background(), []Entry{sampleEntry("r1"), limited}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	rows, err := s.db.Query("SELECT request_id, time, agent, tool, arguments, decision, reason, rule, status, error_code, latency_us FROM audit_log ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, tm, agent, tool, args, decision, reason, rule, status string
		var code sql.NullInt64
		var latency int64
		if err := rows.Scan(&id, &tm, &agent, &tool, &args, &decision, &reason, &rule, &status, &code, &latency); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%v|%d", id, tm, agent, tool, args, decision, reason, rule, status, code, latency))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`r1|2026-11-14T10:02:59.033000000Z|ci-bot|fs__write_file|{"path":"/tmp/x","token":"[REDACTED]"}|allowed|role "writer" allows fs__write_file|writer: fs__write_file|ok|{0 false}|1500`,
		`r2|2026-11-14T10:02:59.033000000Z|ci-bot|fs__write_file|{"path":"/tmp/x","token":"[REDACTED]"}|rate_limited|role "writer" allows fs__write_file|writer: fs__write_file|rpc_error|{-31029 true}|0`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSQLiteTimesSortAsText(t *testing.T) {
	s, _ := openTestStore(t)
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	// Written out of order; with trimmed zeros "…00.5Z" would sort after "…00.123456789Z".
	times := []time.Time{base.Add(500 * time.Millisecond), base.Add(123456789), base, base.Add(time.Second)}
	var entries []Entry
	for i, tm := range times {
		e := sampleEntry(fmt.Sprint(i))
		e.Time = tm.In(time.FixedZone("UTC+5", 5*3600)) // stored as UTC whatever the zone
		entries = append(entries, e)
	}
	if err := s.Write(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT request_id FROM audit_log ORDER BY time")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if got := strings.Join(order, ","); got != "2,1,0,3" {
		t.Errorf("ORDER BY time = %s, want 2,1,0,3", got)
	}
}

func TestSQLiteBatchIsAllOrNothing(t *testing.T) {
	s, _ := openTestStore(t)
	bad := sampleEntry("bad")
	bad.Decision = "maybe"
	err := s.Write(context.Background(), []Entry{sampleEntry("good"), bad})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("Write() error = %v, want one naming the bad entry", err)
	}
	if n := count(t, s.db); n != 0 {
		t.Errorf("%d rows saved, want 0: a failed batch must save nothing", n)
	}
}

func TestSQLiteRetriedBatchIsNotDuplicated(t *testing.T) {
	s, _ := openTestStore(t)
	batch := []Entry{sampleEntry("a"), sampleEntry("b")}
	for range 2 {
		if err := s.Write(context.Background(), batch); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	if n := count(t, s.db); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

func TestSQLiteIsAppendOnly(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.Write(context.Background(), []Entry{sampleEntry("a")}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"UPDATE audit_log SET decision = 'denied'",
		"DELETE FROM audit_log",
	} {
		_, err := s.db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: error = %v, want append-only", stmt, err)
		}
	}
	if n := count(t, s.db); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
}

func TestSQLiteReopenKeepsEntries(t *testing.T) {
	s, path := openTestStore(t)
	if err := s.Write(context.Background(), []Entry{sampleEntry("a")}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	again, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer again.Close()
	if n := count(t, again.db); n != 1 {
		t.Errorf("%d rows after reopen, want 1", n)
	}
}

func TestSQLiteRefusesUnknownSchema(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenSQLite(context.Background(), path); err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Fatalf("OpenSQLite() error = %v, want a schema version error", err)
	}
}

func TestSQLiteRejectsBadPaths(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{
		"",
		filepath.Join(dir, "a?mode=ro"),
		"file:" + filepath.Join(dir, "a.db"),
		filepath.Join(dir, "no-such-dir", "audit.db"),
	} {
		if s, err := OpenSQLite(context.Background(), path); err == nil {
			s.Close()
			t.Errorf("OpenSQLite(%q) error = nil, want one", path)
		}
	}
}

func TestRecorderWithSQLite(t *testing.T) {
	s, _ := openTestStore(t)
	r := NewRecorder(s, Options{QueueSize: 16, ReserveWait: 5 * time.Second})
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Go(func() {
			slot, err := r.Reserve(context.Background())
			if err != nil {
				t.Errorf("Reserve error = %v", err)
				return
			}
			slot.Commit(sampleEntry(fmt.Sprint(i)))
		})
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if n := count(t, s.db); n != 300 {
		t.Errorf("%d rows, want 300", n)
	}
}

func TestSQLiteStoresUTC(t *testing.T) {
	s, _ := openTestStore(t)
	e := sampleEntry("a")
	e.Time = time.Date(2026, 11, 14, 15, 32, 59, 0, time.FixedZone("IST", 5*3600+1800))
	if err := s.Write(context.Background(), []Entry{e}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow("SELECT time FROM audit_log").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "2026-11-14T10:02:59.000000000Z" {
		t.Errorf("stored %s, want the UTC time 2026-11-14T10:02:59.000000000Z", stored)
	}
}
