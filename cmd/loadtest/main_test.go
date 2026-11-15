package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/loadtest"
)

// Built once in TestMain.
var fakeUpstreamBin, gatewayBin string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "loadtest-cmd-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create temp dir:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	fakeUpstreamBin = filepath.Join(dir, "fakeupstream")
	gatewayBin = filepath.Join(dir, "gateway")
	for out, pkg := range map[string]string{
		fakeUpstreamBin: "../../testdata/fakeupstream",
		gatewayBin:      "../gateway",
	} {
		if output, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, output)
			return 1
		}
	}
	return m.Run()
}

func TestRunRoundsSummaryAndSavedResults(t *testing.T) {
	t.Setenv("MCP_GATEWAY_API_KEY", "") // run sets it; restored after the test
	out := filepath.Join(t.TempDir(), "results.json")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-gateway", gatewayBin, "-upstream", fakeUpstreamBin, "-rounds", "2",
		"-n", "30", "-warmup", "5", "-clients", "1,4", "-window", "200ms", "-http-warmup", "50ms", "-out", out}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		`round 1 of 2 \(direct first\): added p50 -?[0-9.]+µs, p99 -?[0-9.]+µs; calls/s by clients 1: [1-9][0-9]*, 4: [1-9]`,
		`round 2 of 2 \(gateway first\)`,
		`Median of 2 rounds`,
		`(?m)^ +p50 +-?[0-9.]+µs +\[`,
		`(?m)^ +4 +[1-9][0-9]* +\[[0-9]+ to [0-9]+\]`,
		`saved ` + regexp.QuoteMeta(out),
	} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Errorf("output has nothing matching %q:\n%s", want, text)
		}
	}
	if key := os.Getenv("MCP_GATEWAY_API_KEY"); key != "" && strings.Contains(text+stderr.String(), key) {
		t.Error("the run's key was printed")
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report loadtest.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("saved results: %v", err)
	}
	if len(report.Rounds) != 2 || report.Machine.GoVersion == "" || report.Settings.Calls != 30 || report.Settings.Policy != "minimal" {
		t.Errorf("report = %+v", report)
	}
	if fmt.Sprint(report.Rounds[0].StdioOrder, report.Rounds[1].StdioOrder) != "[direct gateway] [gateway direct]" {
		t.Errorf("stdio order %v, %v; want it to flip each round", report.Rounds[0].StdioOrder, report.Rounds[1].StdioOrder)
	}
	for i, r := range report.Rounds {
		if len(r.Direct.Samples) != 30 || len(r.Gateway.Samples) != 30 || len(r.HTTP) != 2 {
			t.Errorf("round %d: %d direct and %d gateway samples, %d HTTP runs; want 30, 30, 2", i+1, len(r.Direct.Samples), len(r.Gateway.Samples), len(r.HTTP))
		}
	}

	// Saved results are never overwritten, and that is checked before measuring.
	stdout.Reset()
	err = run(context.Background(), []string{"-gateway", gatewayBin, "-upstream", fakeUpstreamBin, "-out", out}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "never overwritten") || stdout.Len() != 0 {
		t.Errorf("second run with the same -out: error %v, output %q", err, stdout.String())
	}
}

func TestRunStopsOnAMissingBinary(t *testing.T) {
	t.Setenv("MCP_GATEWAY_API_KEY", "")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-gateway", filepath.Join(t.TempDir(), "nope"), "-upstream", fakeUpstreamBin, "-n", "5", "-warmup", "0"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Errorf("error = %v, want one naming the gateway", err)
	}
	if strings.Contains(stdout.String(), "Median of") {
		t.Errorf("printed results after a failed run:\n%s", stdout.String())
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-n", "0"}, &stdout, &stderr); err == nil {
		t.Error("-n 0: no error")
	}
	if err := run(context.Background(), []string{"-rounds", "0"}, &stdout, &stderr); err == nil {
		t.Error("-rounds 0: no error")
	}
	for _, clients := range []string{"0", "1,x", "", "4,-2"} {
		if err := run(context.Background(), []string{"-clients", clients}, &stdout, &stderr); err == nil {
			t.Errorf("-clients %q: no error", clients)
		}
	}
	if err := run(context.Background(), []string{"extra"}, &stdout, &stderr); err == nil {
		t.Error("extra argument: no error")
	}
	if err := run(context.Background(), []string{"-h"}, &stdout, &stderr); err != flag.ErrHelp {
		t.Errorf("-h: error = %v, want flag.ErrHelp", err)
	}
}

func TestPrintSummary(t *testing.T) {
	round := func(directP50, gatewayP50, directP99, gatewayP99 time.Duration, rate1, rate32 float64) loadtest.Round {
		return loadtest.Round{
			Direct:  loadtest.StdioRun{Summary: loadtest.Summary{P50: directP50, P99: directP99}},
			Gateway: loadtest.StdioRun{Summary: loadtest.Summary{P50: gatewayP50, P99: gatewayP99}},
			HTTP: []loadtest.Throughput{
				{Clients: 1, PerSecond: rate1, Calls: 50000, Latency: loadtest.Summary{P99: 300 * time.Microsecond}},
				{Clients: 32, PerSecond: rate32, Calls: 80000, Refused: 3, Latency: loadtest.Summary{P99: 15 * time.Millisecond}},
			},
		}
	}
	report := loadtest.Report{Rounds: []loadtest.Round{
		round(22*time.Microsecond, 64*time.Microsecond, 35*time.Microsecond, 190*time.Microsecond, 9700, 16000),
		round(21*time.Microsecond, 107*time.Microsecond, 60*time.Microsecond, 280*time.Microsecond, 9500, 15000),
		round(22*time.Microsecond, 65*time.Microsecond, 34*time.Microsecond, 183*time.Microsecond, 9800, 18000),
	}}
	var out bytes.Buffer
	if err := printSummary(&out, report); err != nil {
		t.Fatal(err)
	}
	want := "\nMedian of 3 rounds, lowest and highest in brackets.\n\n" +
		"Added by the gateway over stdio, one call at a time:\n" +
		"  p50   43.0µs    [42.0µs to 86.0µs]\n" +
		"  p99  155.0µs  [149.0µs to 220.0µs]\n" +
		"\nHTTP throughput:\n" +
		"  clients  calls/s  [lowest to highest]        p99      refused\n" +
		"        1     9700       [9500 to 9800]    300.0µs  0 of 150000\n" +
		"       32    16000     [15000 to 18000]  15000.0µs  9 of 240009\n" +
		"Refused calls found the audit log full: the gateway fails closed. They are left out of calls/s and p99.\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestPrintRound(t *testing.T) {
	r := loadtest.Round{
		StdioOrder: []string{"gateway", "direct"},
		Direct:     loadtest.StdioRun{Summary: loadtest.Summary{P50: 22 * time.Microsecond, P99: 35 * time.Microsecond}},
		Gateway:    loadtest.StdioRun{Summary: loadtest.Summary{P50: 64500 * time.Nanosecond, P99: 190 * time.Microsecond}},
		HTTP:       []loadtest.Throughput{{Clients: 1, PerSecond: 9752.4}, {Clients: 8, PerSecond: 12641, Refused: 4}},
	}
	var out bytes.Buffer
	if err := printRound(&out, 2, 5, r); err != nil {
		t.Fatal(err)
	}
	if want := "round 2 of 5 (gateway first): added p50 42.5µs, p99 155.0µs; calls/s by clients 1: 9752, 8: 12641 (4 refused)\n"; out.String() != want {
		t.Errorf("got  %q\nwant %q", out.String(), want)
	}
}

func TestShortCommit(t *testing.T) {
	for _, tc := range []struct {
		m    loadtest.Machine
		want string
	}{
		{loadtest.Machine{Commit: "0123456789abcdef0123"}, "0123456789ab"},
		{loadtest.Machine{Commit: "0123456789abcdef0123", Modified: true}, "0123456789ab (with uncommitted changes)"},
		{loadtest.Machine{Commit: "unknown"}, "unknown"},
	} {
		if got := shortCommit(tc.m); got != tc.want {
			t.Errorf("shortCommit(%+v) = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestParseClients(t *testing.T) {
	got, err := parseClients("1, 8,32,128")
	if err != nil || fmt.Sprint(got) != "[1 8 32 128]" {
		t.Errorf("parseClients = %v, %v; want [1 8 32 128]", got, err)
	}
}

func TestPrintThroughput(t *testing.T) {
	results := []loadtest.Throughput{
		{Clients: 1, Calls: 50000, Window: 5 * time.Second, PerSecond: 10000, Latency: loadtest.Summary{P50: 90 * time.Microsecond, P90: 110 * time.Microsecond, P99: 200 * time.Microsecond, Max: 2 * time.Millisecond}},
		{Clients: 32, Calls: 150001, Refused: 7, Window: 5 * time.Second, PerSecond: 30000.2, Latency: loadtest.Summary{P50: time.Millisecond, P90: 1500 * time.Microsecond, P99: 3 * time.Millisecond, Max: 9 * time.Millisecond}},
	}
	var out bytes.Buffer
	if err := printThroughput(&out, results); err != nil {
		t.Fatal(err)
	}
	want := "  HTTP clients  calls/s   calls  refused       p50       p90       p99       max\n" +
		"             1    10000   50000        0    90.0µs   110.0µs   200.0µs  2000.0µs\n" +
		"            32    30000  150001        7  1000.0µs  1500.0µs  3000.0µs  9000.0µs\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// A run that fails still shows the rows that finished before it.
func TestRunEachPrintsFinishedRowsWhenALaterRunFails(t *testing.T) {
	var stdout bytes.Buffer
	_, err := runEach(&stdout, []int{1, 8, 128}, func(clients int) (loadtest.Throughput, error) {
		if clients == 128 {
			return loadtest.Throughput{}, errors.New("error reply -32603: audit log unavailable")
		}
		return loadtest.Throughput{Clients: clients, Calls: 10, PerSecond: 2}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP with 128 clients: error reply -32603") {
		t.Errorf("error = %v, want the 128-client failure", err)
	}
	for _, row := range []string{`(?m)^ +1 +2 +10 `, `(?m)^ +8 +2 +10 `} {
		if !regexp.MustCompile(row).MatchString(stdout.String()) {
			t.Errorf("no row matching %q:\n%s", row, stdout.String())
		}
	}
}

func TestRunEachReturnsResultsAndPrintsNothingWhenAllPass(t *testing.T) {
	var stdout bytes.Buffer
	got, err := runEach(&stdout, []int{1, 8}, func(clients int) (loadtest.Throughput, error) {
		return loadtest.Throughput{Clients: clients}, nil
	})
	if err != nil || len(got) != 2 || got[0].Clients != 1 || got[1].Clients != 8 || stdout.Len() != 0 {
		t.Errorf("got %+v, %v, output %q", got, err, stdout.String())
	}
}

func TestRunEachPrintsNothingWhenTheFirstRunFails(t *testing.T) {
	var stdout bytes.Buffer
	_, err := runEach(&stdout, []int{1}, func(int) (loadtest.Throughput, error) {
		return loadtest.Throughput{}, errors.New("boom")
	})
	if err == nil || stdout.Len() != 0 {
		t.Errorf("error %v, output %q; want an error and no table", err, stdout.String())
	}
}

func TestWriteReportNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(path, loadtest.Report{}); err == nil {
		t.Error("no error writing over an existing file")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep" {
		t.Errorf("file now %q", data)
	}
}
