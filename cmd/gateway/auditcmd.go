package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/audit"
)

// maxShownArgs caps the arguments column in text output. -json shows them
// whole.
const maxShownArgs = 80

// runAudit prints rows from the audit log. It opens the file read-only, so
// it can run next to a live gateway and can't change the log.
func runAudit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gateway audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gateway audit -db audit.db [-agent NAME] [-tool NAME] [-decision D] [-since 1h] [-limit N] [-json]")
		fs.PrintDefaults()
	}
	db := fs.String("db", "", "path to the audit log (required)")
	agent := fs.String("agent", "", "only this agent")
	tool := fs.String("tool", "", "only this tool, or upstream__* for one upstream")
	decision := fs.String("decision", "", "only allowed, denied, or rate_limited")
	since := fs.String("since", "", "only rows from this long ago (1h, 30m) or after this time (RFC 3339)")
	limit := fs.Int("limit", audit.DefaultQueryLimit, "show the newest N rows")
	asJSON := fs.Bool("json", false, "print one JSON object per line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return usageFailure(fs, "unexpected arguments")
	}
	if *db == "" {
		return usageFailure(fs, "missing -db")
	}
	if *limit <= 0 {
		return usageFailure(fs, "-limit must be positive")
	}
	sinceTime, err := parseSince(*since, time.Now())
	if err != nil {
		return usageFailure(fs, "-since: "+err.Error())
	}

	records, err := audit.Query(ctx, *db, audit.Filter{
		Agent:    *agent,
		Tool:     *tool,
		Decision: audit.Decision(*decision),
		Since:    sinceTime,
		Limit:    *limit,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(stdout, records)
	}
	return printTable(stdout, records)
}

// parseSince reads a duration back from now, or an RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, errors.New("duration must be positive")
		}
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither a duration like 1h nor a time like 2026-11-14T10:00:00Z", s)
	}
	return t, nil
}

// jsonRecord is the -json shape: plain names, latency in microseconds.
type jsonRecord struct {
	Seq       int64           `json:"seq"`
	RequestID string          `json:"request_id"`
	Time      string          `json:"time"`
	Agent     string          `json:"agent"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Decision  string          `json:"decision"`
	Reason    string          `json:"reason"`
	Rule      string          `json:"rule"`
	Status    string          `json:"status"`
	ErrorCode *int            `json:"error_code,omitempty"`
	LatencyUs int64           `json:"latency_us"`
}

func printJSON(w io.Writer, records []audit.Record) error {
	enc := json.NewEncoder(w)
	for _, r := range records {
		jr := jsonRecord{
			Seq: r.Seq, RequestID: r.RequestID, Time: r.Time.UTC().Format(audit.TimeLayout),
			Agent: r.Agent, Tool: r.Tool, Arguments: r.Arguments,
			Decision: string(r.Decision), Reason: r.Reason, Rule: r.Rule, Status: string(r.Status),
			LatencyUs: r.Latency.Microseconds(),
		}
		if r.Status == audit.StatusRPCError {
			jr.ErrorCode = &r.ErrorCode
		}
		if err := enc.Encode(jr); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	return nil
}

func printTable(w io.Writer, records []audit.Record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tAGENT\tTOOL\tDECISION\tSTATUS\tLATENCY\tREASON\tARGUMENTS")
	for _, r := range records {
		status := string(r.Status)
		if r.Status == audit.StatusRPCError {
			status += " " + strconv.Itoa(r.ErrorCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Time.UTC().Format(time.RFC3339), printable(r.Agent), printable(r.Tool), r.Decision, status,
			r.Latency.Round(time.Microsecond), printable(r.Reason), printable(shorten(string(r.Arguments), maxShownArgs)))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// printable escapes control characters. Tool names, reasons, and
// arguments come from agents, and printing raw escape sequences to a
// terminal would let them recolor or rewrite what an operator sees. Tabs
// are escaped too, since they would break the columns.
func printable(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// shorten cuts s to at most n runes, marking the cut.
func shorten(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
