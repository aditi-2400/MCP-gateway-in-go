package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var queryBase = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

// seedLog writes six entries a minute apart and returns the file path.
func seedLog(t *testing.T) string {
	t.Helper()
	s, path := openTestStore(t)
	rows := []struct {
		agent, tool string
		decision    Decision
	}{
		{"ci-bot", "fs__read_file", Allowed},
		{"ci-bot", "fs__write_file", Denied},
		{"analyst", "db__query", Allowed},
		{"analyst", "db__query", RateLimited},
		{"ci-bot", "fs__read_file", Allowed},
		{"analyst", "fsx__read", Allowed},
	}
	var entries []Entry
	for i, r := range rows {
		e := sampleEntry(string(rune('a' + i)))
		e.Time = queryBase.Add(time.Duration(i) * time.Minute)
		e.Agent, e.Tool, e.Decision = r.agent, r.tool, r.decision
		entries = append(entries, e)
	}
	if err := s.Write(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	return path
}

func ids(records []Record) string {
	var out []string
	for _, r := range records {
		out = append(out, r.RequestID)
	}
	return strings.Join(out, ",")
}

func TestQueryFilters(t *testing.T) {
	path := seedLog(t)
	tests := []struct {
		name string
		f    Filter
		want string
	}{
		{name: "everything, oldest first", f: Filter{}, want: "a,b,c,d,e,f"},
		{name: "agent", f: Filter{Agent: "analyst"}, want: "c,d,f"},
		{name: "exact tool", f: Filter{Tool: "fs__read_file"}, want: "a,e"},
		{name: "upstream wildcard", f: Filter{Tool: "fs__*"}, want: "a,b,e"},
		{name: "decision", f: Filter{Decision: Allowed}, want: "a,c,e,f"},
		{name: "since", f: Filter{Since: queryBase.Add(3 * time.Minute)}, want: "d,e,f"},
		{name: "since in another zone", f: Filter{Since: queryBase.Add(3 * time.Minute).In(time.FixedZone("EST", -5*3600))}, want: "d,e,f"},
		{name: "newest n", f: Filter{Limit: 2}, want: "e,f"},
		{name: "combined", f: Filter{Agent: "ci-bot", Tool: "fs__*", Decision: Allowed, Limit: 1}, want: "e"},
		{name: "nothing matches", f: Filter{Agent: "nobody"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Query(context.Background(), path, tt.f)
			if err != nil {
				t.Fatalf("Query() error = %v", err)
			}
			if ids(got) != tt.want {
				t.Errorf("Query(%+v) = %s, want %s", tt.f, ids(got), tt.want)
			}
		})
	}
}

func TestQueryReadsEveryField(t *testing.T) {
	s, path := openTestStore(t)
	e := sampleEntry("x")
	e.Status, e.ErrorCode = StatusRPCError, -31029
	if err := s.Write(context.Background(), []Entry{e}); err != nil {
		t.Fatal(err)
	}
	got, err := Query(context.Background(), path, Filter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("Query() = %v, %v", got, err)
	}
	r := got[0]
	if r.Seq != 1 || !r.Time.Equal(e.Time) || r.Agent != e.Agent || r.Tool != e.Tool || string(r.Arguments) != string(e.Arguments) ||
		r.Decision != e.Decision || r.Reason != e.Reason || r.Rule != e.Rule || r.Status != e.Status || r.ErrorCode != -31029 || r.Latency != e.Latency {
		t.Errorf("record = %+v\nwant entry %+v", r, e)
	}
}

func TestQueryRejectsBadFilters(t *testing.T) {
	path := seedLog(t)
	for _, f := range []Filter{
		{Decision: "maybe"},
		{Limit: -1},
		{Limit: maxQueryLimit + 1},
		{Tool: "fs__read_*"},
		{Tool: "*"},
	} {
		if _, err := Query(context.Background(), path, f); err == nil {
			t.Errorf("Query(%+v) error = nil, want one", f)
		}
	}
}

func TestQueryNeverCreatesOrWrites(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := Query(context.Background(), missing, Filter{}); err == nil {
		t.Fatal("Query on a missing file: error = nil")
	}
	if _, err := Query(context.Background(), missing, Filter{}); err == nil || !strings.Contains(err.Error(), "missing.db") {
		t.Errorf("error = %v, want it to name the file", err)
	}

	path := seedLog(t)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"INSERT INTO audit_log (request_id, time, agent, tool, arguments, decision, reason, rule, status, latency_us) VALUES ('z', 't', 'a', 't', 'null', 'allowed', '', '', 'ok', 0)",
		"CREATE TABLE sneaky (x)",
		"DROP TRIGGER audit_log_no_delete",
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%s: worked on the read-only connection", stmt)
		}
	}
}

// The read-only side builds a file: URI, so the path must be escaped. Without
// that, SQLite would decode "%41" in a folder name to "A" and open another
// file.
func TestQueryHandlesOddPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my logs %41 100%")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit log.db")
	s, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write(context.Background(), []Entry{sampleEntry("odd")}); err != nil {
		t.Fatal(err)
	}
	got, err := Query(context.Background(), path, Filter{})
	if err != nil || ids(got) != "odd" {
		t.Fatalf("Query() = %s, %v; want the one row", ids(got), err)
	}
}
