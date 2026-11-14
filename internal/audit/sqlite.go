package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// TimeLayout is how times are stored: UTC with all nine fractional digits,
// so the text always has the same width and sorting it sorts by time.
// time.RFC3339Nano trims trailing zeros, which would break that. The "Z"
// is literal text, so times must be converted with UTC() first, or a local
// clock time would be stored with a false "Z".
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

// schemaVersion is stored in SQLite's user_version, so a later change to
// the table can tell an old file apart.
const schemaVersion = 1

// busyTimeoutMs is how long a write waits for a reader (like the audit
// query command) to let go of the file before failing.
const busyTimeoutMs = 5000

const schema = `
CREATE TABLE IF NOT EXISTS audit_log (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id  TEXT    NOT NULL UNIQUE,
	time        TEXT    NOT NULL,
	agent       TEXT    NOT NULL,
	tool        TEXT    NOT NULL,
	arguments   TEXT    NOT NULL,
	decision    TEXT    NOT NULL CHECK (decision IN ('allowed', 'denied', 'rate_limited')),
	reason      TEXT    NOT NULL,
	rule        TEXT    NOT NULL,
	status      TEXT    NOT NULL CHECK (status IN ('ok', 'tool_error', 'rpc_error', 'no_response')),
	error_code  INTEGER,
	latency_us  INTEGER NOT NULL CHECK (latency_us >= 0)
);
CREATE INDEX IF NOT EXISTS audit_log_time       ON audit_log (time);
CREATE INDEX IF NOT EXISTS audit_log_agent_time ON audit_log (agent, time);
CREATE INDEX IF NOT EXISTS audit_log_tool_time  ON audit_log (tool, time);
CREATE TRIGGER IF NOT EXISTS audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
`

// A retried batch may already be saved (the error can come after the
// commit), so a request ID already in the table is skipped. Only that
// conflict is skipped: INSERT OR IGNORE would also silently drop rows that
// break a CHECK, hiding bugs.
const insert = `
INSERT INTO audit_log (request_id, time, agent, tool, arguments, decision, reason, rule, status, error_code, latency_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (request_id) DO NOTHING`

// SQLiteStore saves entries in a SQLite file. It is a Store.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens or creates the audit database at path and makes sure
// the table, indexes, and append-only triggers exist.
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("audit: database path is empty")
	}
	if strings.ContainsAny(path, "?#") || strings.HasPrefix(path, "file:") {
		// The driver reads "?" as the start of options, and "file:" as a URI.
		return nil, fmt.Errorf("audit: database path %q must be a plain file name", path)
	}
	// WAL lets the query command read while the gateway writes.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, busyTimeoutMs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	// One writer goroutine is all the gateway has, and the driver's docs
	// warn that the default pool has no limit.
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		return nil, errors.Join(fmt.Errorf("audit: %s: %w", path, err), db.Close())
	}
	return &SQLiteStore{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf("schema version %d, this gateway knows %d", version, schemaVersion)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

// Write saves a batch in one transaction: all of it or none.
func (s *SQLiteStore) Write(ctx context.Context, entries []Entry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("audit: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		return errors.Join(fmt.Errorf("audit: prepare: %w", err), tx.Rollback())
	}
	for _, e := range entries {
		var errorCode any // NULL unless the call ended in a JSON-RPC error
		if e.Status == StatusRPCError {
			errorCode = e.ErrorCode
		}
		args := string(e.Arguments)
		if args == "" {
			args = "null"
		}
		if _, err := stmt.ExecContext(ctx,
			e.RequestID, e.Time.UTC().Format(TimeLayout), e.Agent, e.Tool, args,
			string(e.Decision), e.Reason, e.Rule, string(e.Status), errorCode, e.Latency.Microseconds(),
		); err != nil {
			return errors.Join(fmt.Errorf("audit: insert %s: %w", e.RequestID, err), tx.Rollback())
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("audit: commit: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
