// Command loadtest measures how much time the gateway adds to a tool call,
// and how many calls a second it handles.
//
// Each round calls the fake upstream's echo tool over stdio, directly and
// through the gateway, one call at a time; the order flips every round so
// neither side always runs on a warmer or cooler machine. Then it measures
// calls per second over HTTP with several clients calling at once. At the
// end it prints the median of the rounds with the lowest and highest, and
// with -out saves everything as JSON.
//
//	loadtest [-gateway bin/gateway] [-upstream bin/fakeupstream] [-rounds 5]
//	         [-n 5000] [-warmup 500] [-clients 1,8,32] [-window 5s]
//	         [-http-warmup 1s] [-policy minimal]
//	         [-out results.json]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/loadtest"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

type options struct {
	gatewayBin, upstreamBin string
	rounds, n, warmup       int
	clients                 []int
	window, httpWarmup      time.Duration
	policy                  string
	out                     string
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	flags := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.gatewayBin, "gateway", "bin/gateway", "path to the gateway binary")
	flags.StringVar(&o.upstreamBin, "upstream", "bin/fakeupstream", "path to the fake upstream binary")
	flags.IntVar(&o.rounds, "rounds", 5, "how many times to repeat every measurement")
	flags.IntVar(&o.n, "n", 5000, "measured stdio calls per target per round")
	flags.IntVar(&o.warmup, "warmup", 500, "stdio calls per target before measuring starts")
	clients := flags.String("clients", "1,8,32", "HTTP clients calling at once, one run per value; past about 64 the audit log fills on a laptop and calls are refused")
	flags.DurationVar(&o.window, "window", 5*time.Second, "how long each HTTP run counts calls")
	flags.DurationVar(&o.httpWarmup, "http-warmup", time.Second, "how long each HTTP run calls before counting")
	flags.StringVar(&o.policy, "policy", loadtest.PolicyMinimal, "gateway policy: "+strings.Join(loadtest.Policies, " or "))
	flags.StringVar(&o.out, "out", "", "save every result as JSON to this new file")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	if flags.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	if o.rounds <= 0 {
		return o, errors.New("-rounds must be at least 1")
	}
	if !slices.Contains(loadtest.Policies, o.policy) {
		return o, fmt.Errorf("-policy %q: want one of %s", o.policy, strings.Join(loadtest.Policies, ", "))
	}
	var err error
	if o.clients, err = parseClients(*clients); err != nil {
		return o, fmt.Errorf("-clients: %w", err)
	}
	// Checked now, so a long run doesn't end by failing to save.
	if o.out != "" {
		if _, err := os.Stat(o.out); !errors.Is(err, fs.ErrNotExist) {
			return o, fmt.Errorf("-out %s: file exists or can't be checked (%v); results are never overwritten", o.out, err)
		}
	}
	return o, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "loadtest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// A key made for this run only, never shown. The gateway reads it from
	// the environment, which upstream.Start passes on to the child.
	key, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	if err := os.Setenv(loadtest.KeyEnv, key); err != nil {
		return fmt.Errorf("set %s: %w", loadtest.KeyEnv, err)
	}
	report := loadtest.Report{
		Started: time.Now().UTC(),
		Machine: loadtest.ThisMachine(),
		Settings: loadtest.Settings{
			Calls: o.n, Warmup: o.warmup, Clients: o.clients,
			Window: o.window, HTTPWarmup: o.httpWarmup, Policy: o.policy,
		},
	}
	m := report.Machine
	fmt.Fprintf(stdout, "%s, %d CPUs, %s/%s, %s, commit %s\n\n", m.CPU, m.CPUs, m.OS, m.Arch, m.GoVersion, shortCommit(m))

	for i := range o.rounds {
		gatewayArgs, err := roundFiles(dir, i+1, o, key)
		if err != nil {
			return err
		}
		round, err := runRound(ctx, stdout, o, gatewayArgs, key, i%2 == 1)
		if err != nil {
			return fmt.Errorf("round %d: %w", i+1, err)
		}
		report.Rounds = append(report.Rounds, round)
		if err := printRound(stdout, i+1, o.rounds, round); err != nil {
			return err
		}
	}
	if err := printSummary(stdout, report); err != nil {
		return err
	}
	if o.out == "" {
		return nil
	}
	if err := writeReport(o.out, report); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "\nsaved %s\n", o.out)
	return err
}

// roundFiles writes round n's gateway files in a fresh folder inside dir, so
// each round starts with an empty audit log and rounds don't affect each
// other. It returns the gateway's arguments.
func roundFiles(dir string, n int, o options, key string) ([]string, error) {
	roundDir := filepath.Join(dir, fmt.Sprintf("round%d", n))
	if err := os.Mkdir(roundDir, 0o700); err != nil {
		return nil, err
	}
	return loadtest.WriteGatewayFiles(roundDir, o.upstreamBin, auth.HashKey(key), o.policy)
}

// runRound measures both stdio targets, gateway first if gatewayFirst, then
// HTTP throughput.
func runRound(ctx context.Context, stdout io.Writer, o options, gatewayArgs []string, key string, gatewayFirst bool) (loadtest.Round, error) {
	direct := loadtest.StdioTarget{Name: "direct", Command: upstream.Command{Path: o.upstreamBin}, Tool: loadtest.Tool}
	gateway := loadtest.StdioTarget{Name: "gateway", Command: upstream.Command{Path: o.gatewayBin, Args: gatewayArgs}, Tool: loadtest.UpstreamName + "__" + loadtest.Tool}
	order := []loadtest.StdioTarget{direct, gateway}
	if gatewayFirst {
		order = []loadtest.StdioTarget{gateway, direct}
	}

	round := loadtest.Round{}
	for _, target := range order {
		measured, err := measureStdio(ctx, target, o.warmup, o.n)
		if err != nil {
			return round, err
		}
		round.StdioOrder = append(round.StdioOrder, target.Name)
		if target.Name == direct.Name {
			round.Direct = measured
		} else {
			round.Gateway = measured
		}
	}

	var err error
	round.HTTP, err = measureThroughput(ctx, stdout, o.gatewayBin, gatewayArgs, key, o.clients, o.httpWarmup, o.window)
	return round, err
}

func measureStdio(ctx context.Context, target loadtest.StdioTarget, warmup, n int) (loadtest.StdioRun, error) {
	// The child's log is shown only if something goes wrong.
	var childLog bytes.Buffer
	times, err := loadtest.MeasureStdio(ctx, target, warmup, n, &childLog)
	if err != nil {
		return loadtest.StdioRun{}, fmt.Errorf("%w\n%s log:\n%s", err, target.Name, childLog.String())
	}
	return loadtest.StdioRun{Summary: loadtest.Summarize(times), Samples: times}, nil
}

// measureThroughput starts the gateway over HTTP and runs Concurrent once
// for each client count.
func measureThroughput(ctx context.Context, stdout io.Writer, gatewayBin string, gatewayArgs []string, key string, clientCounts []int, warmup, window time.Duration) (results []loadtest.Throughput, err error) {
	var gatewayLog bytes.Buffer
	gw, err := loadtest.StartHTTPGateway(gatewayBin, gatewayArgs, &gatewayLog)
	if err != nil {
		return nil, fmt.Errorf("%w\ngateway log:\n%s", err, gatewayLog.String())
	}
	defer func() {
		if stopErr := gw.Stop(); stopErr != nil && err == nil {
			err = fmt.Errorf("%w\ngateway log:\n%s", stopErr, gatewayLog.String())
		}
	}()

	target := loadtest.HTTPTarget{URL: gw.URL, Key: key, Tool: loadtest.UpstreamName + "__" + loadtest.Tool}
	return runEach(stdout, clientCounts, func(clients int) (loadtest.Throughput, error) {
		client := loadtest.NewHTTPClient(clients)
		defer client.CloseIdleConnections()
		return loadtest.Concurrent(ctx, client, target, clients, warmup, window)
	})
}

// runEach measures each client count in turn. If a run fails, the rows that
// finished are printed before the error is returned.
func runEach(stdout io.Writer, clientCounts []int, measure func(clients int) (loadtest.Throughput, error)) ([]loadtest.Throughput, error) {
	results := make([]loadtest.Throughput, 0, len(clientCounts))
	for _, clients := range clientCounts {
		result, err := measure(clients)
		if err != nil {
			err = fmt.Errorf("HTTP with %d clients: %w", clients, err)
			if len(results) > 0 {
				err = errors.Join(printThroughput(stdout, results), err)
			}
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

// parseClients reads a list like "1,8,32,128".
func parseClients(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a positive number", part)
		}
		out = append(out, n)
	}
	return out, nil
}

// writeReport saves report as JSON in a new file; it never overwrites one.
func writeReport(path string, report loadtest.Report) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode results: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("save results: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return errors.Join(fmt.Errorf("save results: %w", err), f.Close())
	}
	return f.Close()
}

func shortCommit(m loadtest.Machine) string {
	c := m.Commit
	if len(c) > 12 {
		c = c[:12]
	}
	if m.Modified {
		c += " (with uncommitted changes)"
	}
	return c
}

func printRound(w io.Writer, n, of int, r loadtest.Round) error {
	var rates []string
	for _, t := range r.HTTP {
		rate := fmt.Sprintf("%d: %.0f", t.Clients, t.PerSecond)
		if t.Refused > 0 {
			rate += fmt.Sprintf(" (%d refused)", t.Refused)
		}
		rates = append(rates, rate)
	}
	_, err := fmt.Fprintf(w, "round %d of %d (%s first): added p50 %s, p99 %s; calls/s by clients %s\n",
		n, of, r.StdioOrder[0],
		us(r.Gateway.Summary.P50-r.Direct.Summary.P50), us(r.Gateway.Summary.P99-r.Direct.Summary.P99),
		strings.Join(rates, ", "))
	return err
}

func printSummary(w io.Writer, report loadtest.Report) error {
	p50, p99 := report.AddedLatency()
	fmt.Fprintf(w, "\nMedian of %d rounds, lowest and highest in brackets.\n\n", len(report.Rounds))
	fmt.Fprintln(w, "Added by the gateway over stdio, one call at a time:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "p50\t%s\t[%s to %s]\t\n", us(p50.Median), us(p50.Min), us(p50.Max))
	fmt.Fprintf(tw, "p99\t%s\t[%s to %s]\t\n", us(p99.Median), us(p99.Min), us(p99.Max))
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nHTTP throughput:")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "clients\tcalls/s\t[lowest to highest]\tp99\trefused\t")
	for _, c := range report.ThroughputByClients() {
		fmt.Fprintf(tw, "%d\t%.0f\t[%.0f to %.0f]\t%s\t%d of %d\t\n", c.Clients, c.PerSecond.Median, c.PerSecond.Min, c.PerSecond.Max,
			us(c.P99.Median), c.Refused, c.Calls+c.Refused)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "Refused calls found the audit log full: the gateway fails closed. They are left out of calls/s and p99.")
	return err
}

func printThroughput(w io.Writer, results []loadtest.Throughput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "HTTP clients\tcalls/s\tcalls\trefused\tp50\tp90\tp99\tmax\t")
	for _, r := range results {
		fmt.Fprintf(tw, "%d\t%.0f\t%d\t%d\t%s\t%s\t%s\t%s\t\n", r.Clients, r.PerSecond, r.Calls, r.Refused,
			us(r.Latency.P50), us(r.Latency.P90), us(r.Latency.P99), us(r.Latency.Max))
	}
	return tw.Flush()
}

// us formats d in microseconds with one decimal, e.g. "123.4µs".
func us(d time.Duration) string {
	return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
}
