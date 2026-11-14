package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DefaultQueryLimit is how many rows a query returns when no limit is given.
const DefaultQueryLimit = 50

// maxQueryLimit caps one query, so a typo can't try to load the whole log.
const maxQueryLimit = 100_000

// Filter picks rows. Zero fields match everything.
type Filter struct {
	Agent    string
	Tool     string // exact name, or upstream__* for every tool of one upstream
	Decision Decision
	Since    time.Time
	Limit    int // the newest Limit rows; 0 means DefaultQueryLimit
}

// Record is a stored entry and its position in the log.
type Record struct {
	Seq int64
	Entry
}

func (f Filter) validate() error {
	var errs []error
	if f.Decision != "" && !slices.Contains([]Decision{Allowed, Denied, RateLimited}, f.Decision) {
		errs = append(errs, fmt.Errorf("decision %q must be allowed, denied, or rate_limited", f.Decision))
	}
	if f.Limit < 0 || f.Limit > maxQueryLimit {
		errs = append(errs, fmt.Errorf("limit must be from 1 to %d", maxQueryLimit))
	}
	if strings.Contains(strings.TrimSuffix(f.Tool, "__*"), "*") {
		errs = append(errs, fmt.Errorf("tool %q: * only works as upstream__*", f.Tool))
	}
	return errors.Join(errs...)
}

// openReadOnly opens the audit log so it can't be changed. SQLite's mode=ro
// only applies to file: URIs (the driver drops it from plain names), and
// query_only refuses writes even if that ever changes. A missing file is
// an error, never created.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("audit: %s: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMs))
	q.Add("_pragma", "query_only(1)")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Query returns the newest rows matching f, oldest first, the way tail
// shows the end of a file.
func Query(ctx context.Context, path string, f Filter) ([]Record, error) {
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if f.Limit == 0 {
		f.Limit = DefaultQueryLimit
	}
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var where []string
	var args []any
	if f.Agent != "" {
		where, args = append(where, "agent = ?"), append(args, f.Agent)
	}
	if upstream, ok := strings.CutSuffix(f.Tool, "*"); ok {
		// Compared as a plain prefix: LIKE would treat the "_" in "__" as a
		// wildcard.
		where, args = append(where, "substr(tool, 1, ?) = ?"), append(args, len(upstream), upstream)
	} else if f.Tool != "" {
		where, args = append(where, "tool = ?"), append(args, f.Tool)
	}
	if f.Decision != "" {
		where, args = append(where, "decision = ?"), append(args, string(f.Decision))
	}
	if !f.Since.IsZero() {
		// Times are fixed-width UTC text, so text order is time order.
		where, args = append(where, "time >= ?"), append(args, f.Since.UTC().Format(TimeLayout))
	}
	query := "SELECT seq, request_id, time, agent, tool, arguments, decision, reason, rule, status, error_code, latency_us FROM audit_log"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY seq DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: query %s: %w", path, err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var tm, argsText, decision, status string
		var code sql.NullInt64
		var latencyUs int64
		if err := rows.Scan(&r.Seq, &r.RequestID, &tm, &r.Agent, &r.Tool, &argsText, &decision, &r.Reason, &r.Rule, &status, &code, &latencyUs); err != nil {
			return nil, fmt.Errorf("audit: read row: %w", err)
		}
		if r.Time, err = time.Parse(TimeLayout, tm); err != nil {
			return nil, fmt.Errorf("audit: row %d: bad time %q: %w", r.Seq, tm, err)
		}
		r.Arguments = json.RawMessage(argsText)
		r.Decision, r.Status = Decision(decision), Status(status)
		r.ErrorCode = int(code.Int64)
		r.Latency = time.Duration(latencyUs) * time.Microsecond
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: read rows: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}
