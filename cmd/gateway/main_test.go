package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const testTimeout = 5 * time.Second

const validMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// fakeUpstreamBin is the fake MCP server binary, built once in TestMain.
var fakeUpstreamBin string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gateway-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create temp dir:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	fakeUpstreamBin = filepath.Join(dir, "fakeupstream")
	build := exec.Command("go", "build", "-o", fakeUpstreamBin, "../../testdata/fakeupstream")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fake upstream: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// syncBuffer is a bytes.Buffer that the gateway's logger and the test can
// use at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// gateway is a running run() call with pipes for the client side.
type gateway struct {
	t         *testing.T
	in        *io.PipeWriter
	responses chan *jsonrpc.Message
	done      chan error
	stderr    *syncBuffer
}

func startGateway(t *testing.T, ctx context.Context, args ...string) *gateway {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	g := &gateway{
		t:         t,
		in:        inW,
		responses: make(chan *jsonrpc.Message, 16),
		done:      make(chan error, 1),
		stderr:    &syncBuffer{},
	}

	go func() {
		g.done <- run(ctx, args, inR, outW, g.stderr)
		outW.Close()
	}()
	go func() {
		r := jsonrpc.NewReader(outR, jsonrpc.DefaultMaxMessageSize)
		for {
			msg, err := r.Read()
			if err != nil {
				close(g.responses)
				return
			}
			g.responses <- msg
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		outR.Close()
	})
	return g
}

func (g *gateway) send(line string) {
	g.t.Helper()
	if _, err := io.WriteString(g.in, line+"\n"); err != nil {
		g.t.Fatalf("client write: %v", err)
	}
}

func (g *gateway) next() *jsonrpc.Message {
	g.t.Helper()
	select {
	case msg, ok := <-g.responses:
		if !ok {
			g.t.Fatal("gateway output closed, want a message")
		}
		return msg
	case <-time.After(testTimeout):
		g.t.Fatal("no message from gateway in time")
		return nil
	}
}

func (g *gateway) wait() error {
	g.t.Helper()
	select {
	case err := <-g.done:
		return err
	case <-time.After(testTimeout):
		g.t.Fatal("run did not return in time")
		return nil
	}
}

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
}

func TestRunForwardsRequestsUntilStdinCloses(t *testing.T) {
	g := startGateway(t, context.Background(), "--", fakeUpstreamBin)

	g.send(`{"jsonrpc":"2.0","id":"list-1","method":"tools/list","params":{` + validMeta + `}}`)
	resp := g.next()
	if resp.ID == nil || resp.ID.String() != `"list-1"` || resp.Error != nil {
		t.Fatalf("tools/list response = id %v, error %v", resp.ID, resp.Error)
	}
	if !strings.Contains(string(resp.Result), `"name":"echo"`) {
		t.Fatalf("tools/list result = %s, want the echo tool", resp.Result)
	}

	g.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"},` + validMeta + `}}`)
	resp = g.next()
	if resp.ID == nil || resp.ID.String() != "7" || !strings.Contains(string(resp.Result), `"text":"hi"`) {
		t.Fatalf("tools/call response = id %v, result %s, error %v", resp.ID, resp.Result, resp.Error)
	}

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v, want nil after stdin EOF\nstderr:\n%s", err, g.stderr)
	}
	if !strings.Contains(g.stderr.String(), "gateway stopped") {
		t.Errorf("stderr = %q, want a stop log line", g.stderr)
	}
}

// Same as `echo request | gateway -- server`: stdin closes right after the
// request, and the reply must still arrive.
func TestRunRepliesWhenStdinClosesRightAway(t *testing.T) {
	g := startGateway(t, context.Background(), "--", fakeUpstreamBin)

	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3},` + validMeta + `}}`)
	g.in.Close()

	if resp := g.next(); resp.Error != nil || !strings.Contains(string(resp.Result), `"sum":5`) {
		t.Fatalf("response = result %s, error %v, want sum 5", resp.Result, resp.Error)
	}
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\nstderr:\n%s", err, g.stderr)
	}
}

func TestRunStopsCleanlyWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := startGateway(t, ctx, "--", fakeUpstreamBin)

	// A round trip proves the gateway is up before it is stopped.
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()

	cancel()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v, want nil on signal\nstderr:\n%s", err, g.stderr)
	}
	if !strings.Contains(g.stderr.String(), "shutting down on signal") {
		t.Errorf("stderr = %q, want a signal log line", g.stderr)
	}
}

func TestRunFailsWhenUpstreamExits(t *testing.T) {
	skipWithoutShell(t)
	g := startGateway(t, context.Background(), "--", "sh", "-c", "exit 0")

	err := g.wait()
	if !errors.Is(err, errUpstreamExited) {
		t.Fatalf("run() error = %v, want errUpstreamExited", err)
	}
}

func TestRunDoesNotLogUpstreamArgs(t *testing.T) {
	g := startGateway(t, context.Background(), "--", fakeUpstreamBin, "--token=secret-value")
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if strings.Contains(g.stderr.String(), "secret-value") {
		t.Fatalf("stderr leaks upstream args: %q", g.stderr)
	}
}

func TestRunStartFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-server")
	err := run(context.Background(), []string{"--", missing}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("run() error = %v, want a start error", err)
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantErr     error
		wantPath    string
		wantArgs    []string
		wantTimeout time.Duration
	}{
		{name: "command only", args: []string{"--", "server"}, wantPath: "server", wantArgs: []string{}, wantTimeout: 60 * time.Second},
		{name: "command with args", args: []string{"--", "server", "-v", "x"}, wantPath: "server", wantArgs: []string{"-v", "x"}, wantTimeout: 60 * time.Second},
		{name: "no separator", args: []string{"server", "-v"}, wantPath: "server", wantArgs: []string{"-v"}, wantTimeout: 60 * time.Second},
		{name: "timeout flag", args: []string{"-call-timeout", "2s", "--", "server"}, wantPath: "server", wantArgs: []string{}, wantTimeout: 2 * time.Second},
		{name: "log level flag", args: []string{"-log-level", "debug", "--", "server"}, wantPath: "server", wantArgs: []string{}, wantTimeout: 60 * time.Second},
		{name: "missing command", args: nil, wantErr: errUsage},
		{name: "only separator", args: []string{"--"}, wantErr: errUsage},
		{name: "zero timeout", args: []string{"-call-timeout", "0s", "--", "server"}, wantErr: errUsage},
		{name: "negative timeout", args: []string{"-call-timeout", "-1s", "--", "server"}, wantErr: errUsage},
		{name: "bad timeout", args: []string{"-call-timeout", "soon", "--", "server"}, wantErr: errUsage},
		{name: "bad log level", args: []string{"-log-level", "loud", "--", "server"}, wantErr: errUsage},
		{name: "unknown flag", args: []string{"-nope", "--", "server"}, wantErr: errUsage},
		{name: "help", args: []string{"-h"}, wantErr: flag.ErrHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseFlags(tt.args, io.Discard)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parseFlags() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags() error = %v", err)
			}
			if cfg.upstream.Path != tt.wantPath || strings.Join(cfg.upstream.Args, " ") != strings.Join(tt.wantArgs, " ") {
				t.Errorf("upstream = %+v, want path %q args %q", cfg.upstream, tt.wantPath, tt.wantArgs)
			}
			if cfg.callTimeout != tt.wantTimeout {
				t.Errorf("callTimeout = %v, want %v", cfg.callTimeout, tt.wantTimeout)
			}
		})
	}
}
