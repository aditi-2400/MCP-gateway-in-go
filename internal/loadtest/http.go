package loadtest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// maxReplySize limits how much of a reply is read. add's replies are tiny.
const maxReplySize = 1 << 20

// startupTimeout bounds how long StartHTTPGateway waits for the gateway to
// say where it listens.
const startupTimeout = 10 * time.Second

// HTTPTarget is a gateway's Streamable HTTP endpoint and the call to send.
type HTTPTarget struct {
	URL  string // e.g. http://127.0.0.1:8080/mcp
	Key  string // API key, sent as a Bearer token
	Tool string // the add tool's name at the gateway
}

// Throughput is the result of one Concurrent run.
type Throughput struct {
	Clients   int           `json:"clients"`
	Calls     int           `json:"calls"`         // calls started in the window
	Window    time.Duration `json:"window_ns"`     // how long calls were counted
	PerSecond float64       `json:"calls_per_sec"` // Calls / Window
	Latency   Summary       `json:"latency"`
}

// NewHTTPClient returns a client that keeps one open connection per load
// client. The default keeps only 2 idle connections per host, so with more
// clients most calls would open a new connection and the run would measure
// TCP setup.
func NewHTTPClient(clients int) *http.Client {
	return &http.Client{Transport: &http.Transport{
		MaxIdleConns:        clients,
		MaxIdleConnsPerHost: clients,
		IdleConnTimeout:     time.Minute,
	}}
}

// Concurrent runs clients goroutines, each sending tools/call to target one
// after another, with no pause in between. Calls during warmup are not
// counted. Then it counts every call started in the next window, and
// stops starting new ones.
//
// The first failed call stops every client and is returned: a non-200
// status, an error reply, or a tool result with isError.
func Concurrent(ctx context.Context, client *http.Client, target HTTPTarget, clients int, warmup, window time.Duration) (Throughput, error) {
	if clients <= 0 || warmup < 0 || window <= 0 {
		return Throughput{}, fmt.Errorf("loadtest: need clients > 0, warmup >= 0, window > 0; got %d, %v, %v", clients, warmup, window)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var nextID atomic.Int64
	measureFrom := time.Now().Add(warmup)
	stopAt := measureFrom.Add(window)
	perClient := make([][]time.Duration, clients)
	var wg sync.WaitGroup
	for c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				start := time.Now()
				if !start.Before(stopAt) {
					return
				}
				if err := postCall(ctx, client, target, nextID.Add(1)); err != nil {
					cancel(err)
					return
				}
				if !start.Before(measureFrom) {
					perClient[c] = append(perClient[c], time.Since(start))
				}
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return Throughput{}, err
	}

	var all []time.Duration
	for _, times := range perClient {
		all = append(all, times...)
	}
	return Throughput{
		Clients:   clients,
		Calls:     len(all),
		Window:    window,
		PerSecond: float64(len(all)) / window.Seconds(),
		Latency:   Summarize(all),
	}, nil
}

// postCall sends one tools/call with the headers the spec asks for, and
// checks the reply.
func postCall(ctx context.Context, client *http.Client, target HTTPTarget, id int64) error {
	body := `{"jsonrpc":"2.0","id":` + strconv.FormatInt(id, 10) + `,"method":"tools/call","params":` + string(CallParams(target.Tool)) + `}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+target.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", target.Tool)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Reading to the end lets the connection be reused.
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxReplySize))
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	msg, err := jsonrpc.Parse(data)
	if err != nil {
		return fmt.Errorf("reply %q: %w", data, err)
	}
	return checkReply(msg, nil)
}

// HTTPGateway is a gateway process serving Streamable HTTP.
type HTTPGateway struct {
	URL  string // the MCP endpoint
	cmd  *exec.Cmd
	logs sync.WaitGroup
}

var listenLine = regexp.MustCompile(`msg="http listening" addr=(\S+)`)

// StartHTTPGateway starts the gateway binary with args plus -http on a free
// loopback port, and waits until it listens. Its log is copied to stderr.
func StartHTTPGateway(bin string, args []string, stderr io.Writer) (*HTTPGateway, error) {
	cmd := exec.Command(bin, append(args, "-http", "127.0.0.1:0")...)
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gateway: %w", err)
	}
	g := &HTTPGateway{cmd: cmd}

	addr := make(chan string, 1)
	g.logs.Add(1)
	go func() {
		defer g.logs.Done()
		defer close(addr)
		sc := bufio.NewScanner(pipe)
		found := false
		for sc.Scan() {
			fmt.Fprintln(stderr, sc.Text())
			if m := listenLine.FindStringSubmatch(sc.Text()); m != nil && !found {
				found = true
				addr <- m[1]
			}
		}
	}()

	select {
	case a, ok := <-addr:
		if ok {
			g.URL = "http://" + a + "/mcp"
			return g, nil
		}
		// The log ended without an address: the gateway exited.
		g.logs.Wait()
		return nil, fmt.Errorf("gateway exited before listening: %w", cmd.Wait())
	case <-time.After(startupTimeout):
		return nil, errors.Join(errors.New("gateway did not start listening in time"), g.kill())
	}
}

// Stop asks the gateway to shut down with SIGTERM and waits for it.
func (g *HTTPGateway) Stop() error {
	if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal gateway: %w", err)
	}
	g.logs.Wait() // the pipe must be drained before Wait closes it
	if err := g.cmd.Wait(); err != nil {
		return fmt.Errorf("gateway exited: %w", err)
	}
	return nil
}

func (g *HTTPGateway) kill() error {
	if err := g.cmd.Process.Kill(); err != nil {
		return err
	}
	g.logs.Wait()
	_ = g.cmd.Wait() // killed on purpose, so its exit status says nothing
	return nil
}
