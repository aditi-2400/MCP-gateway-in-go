// Command loadtest measures how much time the gateway adds to a tool call.
//
// First it calls the fake upstream's add tool over stdio, directly and then
// through the gateway, one call at a time, and prints the latency of each.
// Then it measures how many calls per second the gateway handles over HTTP
// with several clients calling at once.
//
//	loadtest [-gateway bin/gateway] [-upstream bin/fakeupstream] [-n 5000] [-warmup 500]
//	         [-clients 1,8,32,128] [-window 5s] [-http-warmup 1s]
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gatewayBin := fs.String("gateway", "bin/gateway", "path to the gateway binary")
	upstreamBin := fs.String("upstream", "bin/fakeupstream", "path to the fake upstream binary")
	n := fs.Int("n", 5000, "measured calls per target")
	warmup := fs.Int("warmup", 500, "calls per target before measuring starts")
	clientsFlag := fs.String("clients", "1,8,32,128", "HTTP clients calling at once, one run per value")
	window := fs.Duration("window", 5*time.Second, "how long each HTTP run counts calls")
	httpWarmup := fs.Duration("http-warmup", time.Second, "how long each HTTP run calls before counting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	clientCounts, err := parseClients(*clientsFlag)
	if err != nil {
		return fmt.Errorf("-clients: %w", err)
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
	gatewayArgs, err := loadtest.WriteGatewayFiles(dir, *upstreamBin, auth.HashKey(key))
	if err != nil {
		return err
	}

	if err := measureLatency(ctx, stdout, *gatewayBin, *upstreamBin, gatewayArgs, *warmup, *n); err != nil {
		return err
	}
	fmt.Fprintln(stdout)
	return measureThroughput(ctx, stdout, *gatewayBin, gatewayArgs, key, clientCounts, *httpWarmup, *window)
}

// measureLatency times stdio calls directly and through the gateway.
func measureLatency(ctx context.Context, stdout io.Writer, gatewayBin, upstreamBin string, gatewayArgs []string, warmup, n int) error {
	targets := []loadtest.StdioTarget{
		{Name: "direct", Command: upstream.Command{Path: upstreamBin}, Tool: "add"},
		{Name: "gateway", Command: upstream.Command{Path: gatewayBin, Args: gatewayArgs}, Tool: loadtest.UpstreamName + "__add"},
	}
	summaries := make([]loadtest.Summary, len(targets))
	for i, target := range targets {
		// The child's log is shown only if something goes wrong.
		var childLog bytes.Buffer
		times, err := loadtest.MeasureStdio(ctx, target, warmup, n, &childLog)
		if err != nil {
			return fmt.Errorf("%w\n%s log:\n%s", err, target.Name, childLog.String())
		}
		summaries[i] = loadtest.Summarize(times)
	}
	return printLatency(stdout, targets, summaries)
}

// measureThroughput starts the gateway over HTTP and runs Concurrent once
// for each client count.
func measureThroughput(ctx context.Context, stdout io.Writer, gatewayBin string, gatewayArgs []string, key string, clientCounts []int, warmup, window time.Duration) (err error) {
	var gatewayLog bytes.Buffer
	gw, err := loadtest.StartHTTPGateway(gatewayBin, gatewayArgs, &gatewayLog)
	if err != nil {
		return fmt.Errorf("%w\ngateway log:\n%s", err, gatewayLog.String())
	}
	defer func() {
		if stopErr := gw.Stop(); stopErr != nil && err == nil {
			err = fmt.Errorf("%w\ngateway log:\n%s", stopErr, gatewayLog.String())
		}
	}()

	target := loadtest.HTTPTarget{URL: gw.URL, Key: key, Tool: loadtest.UpstreamName + "__add"}
	return runEach(stdout, clientCounts, func(clients int) (loadtest.Throughput, error) {
		client := loadtest.NewHTTPClient(clients)
		defer client.CloseIdleConnections()
		return loadtest.Concurrent(ctx, client, target, clients, warmup, window)
	})
}

// runEach measures each client count in turn and prints the table. If a run
// fails, the rows that finished are still printed before the error.
func runEach(stdout io.Writer, clientCounts []int, measure func(clients int) (loadtest.Throughput, error)) error {
	results := make([]loadtest.Throughput, 0, len(clientCounts))
	for _, clients := range clientCounts {
		result, err := measure(clients)
		if err != nil {
			err = fmt.Errorf("HTTP with %d clients: %w", clients, err)
			if len(results) > 0 {
				err = errors.Join(printThroughput(stdout, results), err)
			}
			return err
		}
		results = append(results, result)
	}
	return printThroughput(stdout, results)
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

func printThroughput(w io.Writer, results []loadtest.Throughput) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "HTTP clients	calls/s	calls	p50	p90	p99	max	")
	for _, r := range results {
		fmt.Fprintf(tw, "%d	%.0f	%d	%s	%s	%s	%s	\n", r.Clients, r.PerSecond, r.Calls,
			us(r.Latency.P50), us(r.Latency.P90), us(r.Latency.P99), us(r.Latency.Max))
	}
	return tw.Flush()
}

func printLatency(w io.Writer, targets []loadtest.StdioTarget, s []loadtest.Summary) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "target\tcalls\tmin\tp50\tp90\tp99\tmax\tmean\t")
	for i, t := range targets {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", t.Name, s[i].N,
			us(s[i].Min), us(s[i].P50), us(s[i].P90), us(s[i].P99), us(s[i].Max), us(s[i].Mean))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	direct, gateway := s[0], s[1]
	_, err := fmt.Fprintf(w, "\nadded by the gateway: p50 %s, p99 %s\n",
		us(gateway.P50-direct.P50), us(gateway.P99-direct.P99))
	return err
}

// us formats d in microseconds with one decimal, e.g. "123.4µs".
func us(d time.Duration) string {
	return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
}
