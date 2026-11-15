package main

import (
	"bytes"
	"context"
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

func TestRunPrintsBothTargets(t *testing.T) {
	t.Setenv("MCP_GATEWAY_API_KEY", "") // run sets it; restored after the test
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-gateway", gatewayBin, "-upstream", fakeUpstreamBin, "-n", "30", "-warmup", "5",
		"-clients", "1,4", "-window", "200ms", "-http-warmup", "50ms"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, row := range []string{`direct +30 `, `gateway +30 `} {
		if !regexp.MustCompile(row).MatchString(out) {
			t.Errorf("output has no row matching %q:\n%s", row, out)
		}
	}
	if !regexp.MustCompile(`added by the gateway: p50 -?[0-9.]+µs, p99 -?[0-9.]+µs`).MatchString(out) {
		t.Errorf("output has no added-latency line:\n%s", out)
	}
	for _, row := range []string{`(?m)^ +1 +[1-9][0-9]* +[1-9]`, `(?m)^ +4 +[1-9][0-9]* +[1-9]`} {
		if !regexp.MustCompile(row).MatchString(out) {
			t.Errorf("output has no throughput row matching %q:\n%s", row, out)
		}
	}
	if key := os.Getenv("MCP_GATEWAY_API_KEY"); key != "" && strings.Contains(out+stderr.String(), key) {
		t.Error("the run's key was printed")
	}
}

func TestRunStopsOnAMissingBinary(t *testing.T) {
	t.Setenv("MCP_GATEWAY_API_KEY", "")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-gateway", filepath.Join(t.TempDir(), "nope"), "-upstream", fakeUpstreamBin, "-n", "5", "-warmup", "0"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Errorf("error = %v, want one naming the gateway", err)
	}
	if strings.Contains(stdout.String(), "added by the gateway") {
		t.Errorf("printed results after a failed run:\n%s", stdout.String())
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-n", "0"}, &stdout, &stderr); err == nil {
		t.Error("-n 0: no error")
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

func TestPrintLatency(t *testing.T) {
	targets := []loadtest.StdioTarget{{Name: "direct"}, {Name: "gateway"}}
	summaries := []loadtest.Summary{
		{N: 3, Min: 10 * time.Microsecond, Mean: 20 * time.Microsecond, P50: 20 * time.Microsecond, P90: 25 * time.Microsecond, P99: 30 * time.Microsecond, Max: 31 * time.Microsecond},
		{N: 3, Min: 50 * time.Microsecond, Mean: 70 * time.Microsecond, P50: 60 * time.Microsecond, P90: 90 * time.Microsecond, P99: 180500 * time.Nanosecond, Max: time.Millisecond},
	}
	var out bytes.Buffer
	if err := printLatency(&out, targets, summaries); err != nil {
		t.Fatal(err)
	}
	want := "   target  calls     min     p50     p90      p99       max    mean\n" +
		"   direct      3  10.0µs  20.0µs  25.0µs   30.0µs    31.0µs  20.0µs\n" +
		"  gateway      3  50.0µs  60.0µs  90.0µs  180.5µs  1000.0µs  70.0µs\n" +
		"\nadded by the gateway: p50 40.0µs, p99 150.5µs\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
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
		{Clients: 32, Calls: 150001, Window: 5 * time.Second, PerSecond: 30000.2, Latency: loadtest.Summary{P50: time.Millisecond, P90: 1500 * time.Microsecond, P99: 3 * time.Millisecond, Max: 9 * time.Millisecond}},
	}
	var out bytes.Buffer
	if err := printThroughput(&out, results); err != nil {
		t.Fatal(err)
	}
	want := "  HTTP clients  calls/s   calls       p50       p90       p99       max\n" +
		"             1    10000   50000    90.0µs   110.0µs   200.0µs  2000.0µs\n" +
		"            32    30000  150001  1000.0µs  1500.0µs  3000.0µs  9000.0µs\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// A run that fails still shows the rows that finished before it.
func TestRunEachPrintsFinishedRowsWhenALaterRunFails(t *testing.T) {
	var stdout bytes.Buffer
	err := runEach(&stdout, []int{1, 8, 128}, func(clients int) (loadtest.Throughput, error) {
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

func TestRunEachPrintsNothingWhenTheFirstRunFails(t *testing.T) {
	var stdout bytes.Buffer
	err := runEach(&stdout, []int{1}, func(int) (loadtest.Throughput, error) {
		return loadtest.Throughput{}, errors.New("boom")
	})
	if err == nil || stdout.Len() != 0 {
		t.Errorf("error %v, output %q; want an error and no table", err, stdout.String())
	}
}
