package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

const testTimeout = 5 * time.Second

const validMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// Binaries built once in TestMain: the fake MCP server, and the gateway
// itself for the end-to-end tests.
var fakeUpstreamBin, gatewayBin string

// testAPIKey belongs to the "tester" agent in every config writeConfig makes.
var testAPIKey string

// testEnv is the environment run sees in tests: just the test key.
func testEnv(name string) string {
	if name == apiKeyEnv {
		return testAPIKey
	}
	return ""
}

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

	if testAPIKey, err = auth.GenerateKey(); err != nil {
		fmt.Fprintln(os.Stderr, "generate test key:", err)
		return 1
	}

	fakeUpstreamBin = filepath.Join(dir, "fakeupstream")
	gatewayBin = filepath.Join(dir, "gateway")
	builds := map[string]string{
		fakeUpstreamBin: "../../testdata/fakeupstream",
		gatewayBin:      ".",
	}
	for out, pkg := range builds {
		build := exec.Command("go", "build", "-o", out, pkg)
		if output, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, output)
			return 1
		}
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
		g.done <- run(ctx, args, inR, outW, g.stderr, testEnv)
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

// upstreamEntry is one upstream in a test config.
type upstreamEntry struct {
	name    string
	command string
	args    []string
}

// writeConfig writes a gateway config to a temp file and returns its path.
// Values are JSON-quoted, which YAML accepts, so shell scripts with quotes
// survive.
func writeConfig(t *testing.T, upstreams ...upstreamEntry) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("upstreams:\n")
	for _, u := range upstreams {
		args, err := json.Marshal(append([]string{}, u.args...))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "  - name: %s\n    command: %q\n    args: %s\n", u.name, u.command, args)
	}
	fmt.Fprintf(&b, "agents:\n  - name: tester\n    key_sha256: %s\n", auth.HashKey(testAPIKey))
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeConfig(t *testing.T, names ...string) string {
	t.Helper()
	entries := make([]upstreamEntry, len(names))
	for i, name := range names {
		entries[i] = upstreamEntry{name: name, command: fakeUpstreamBin}
	}
	return writeConfig(t, entries...)
}

// discoverThenExit is an upstream that passes the startup check (always
// request id 1 on a new connection) and then exits.
var discoverThenExit = upstreamEntry{name: "flaky", command: "sh", args: []string{"-c",
	`read line; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}'; sleep 0.2`}}

func TestRunRoutesAcrossUpstreamsUntilStdinCloses(t *testing.T) {
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "alpha", "beta"))

	g.send(`{"jsonrpc":"2.0","id":"list-1","method":"tools/list","params":{` + validMeta + `}}`)
	resp := g.next()
	if resp.ID == nil || resp.ID.String() != `"list-1"` || resp.Error != nil {
		t.Fatalf("tools/list response = id %v, error %v", resp.ID, resp.Error)
	}
	for _, want := range []string{`"name":"alpha__echo"`, `"name":"beta__echo"`} {
		if !strings.Contains(string(resp.Result), want) {
			t.Fatalf("tools/list result = %s, want %s", resp.Result, want)
		}
	}

	g.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"beta__echo","arguments":{"text":"hi"},` + validMeta + `}}`)
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

// Same as `echo request | gateway -config ...`: stdin closes right after the
// request, and the reply must still arrive.
func TestRunRepliesWhenStdinClosesRightAway(t *testing.T) {
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"))

	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__add","arguments":{"a":2,"b":3},` + validMeta + `}}`)
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
	g := startGateway(t, ctx, "-config", fakeConfig(t, "fake"))

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

func TestRunFailsWhenAnUpstreamExits(t *testing.T) {
	skipWithoutShell(t)
	cfg := writeConfig(t, upstreamEntry{name: "steady", command: fakeUpstreamBin}, discoverThenExit)
	g := startGateway(t, context.Background(), "-config", cfg)

	err := g.wait()
	if !errors.Is(err, errUpstreamExited) || !strings.Contains(err.Error(), `"flaky"`) {
		t.Fatalf("run() error = %v, want errUpstreamExited naming \"flaky\"", err)
	}
}

func TestRunRefusesToStartWhenAnUpstreamFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-server")
	cfg := writeConfig(t,
		upstreamEntry{name: "good", command: fakeUpstreamBin},
		upstreamEntry{name: "broken", command: missing},
	)
	err := run(context.Background(), []string{"-config", cfg}, strings.NewReader(""), io.Discard, io.Discard, testEnv)
	if err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Fatalf("run() error = %v, want an error naming \"broken\"", err)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("upstreams:\n  - name: Bad_Name\n    command: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{missing, bad} {
		err := run(context.Background(), []string{"-config", path}, strings.NewReader(""), io.Discard, io.Discard, testEnv)
		if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), path) {
			t.Errorf("run(%s) error = %v, want a config error naming the file", path, err)
		}
	}
}

func TestRunDoesNotLogUpstreamArgs(t *testing.T) {
	cfg := writeConfig(t, upstreamEntry{name: "fake", command: fakeUpstreamBin, args: []string{"--token=secret-value"}})
	g := startGateway(t, context.Background(), "-config", cfg)
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if strings.Contains(g.stderr.String(), "secret-value") {
		t.Fatalf("stderr leaks upstream args: %q", g.stderr)
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantErr     error
		wantConfig  string
		wantTimeout time.Duration
		wantHTTP    string
		wantOrigins string
	}{
		{name: "config only", args: []string{"-config", "g.yaml"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second},
		{name: "config with equals", args: []string{"-config=g.yaml"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second},
		{name: "timeout flag", args: []string{"-config", "g.yaml", "-call-timeout", "2s"}, wantConfig: "g.yaml", wantTimeout: 2 * time.Second},
		{name: "log level flag", args: []string{"-log-level", "debug", "-config", "g.yaml"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second},
		{name: "missing config", args: nil, wantErr: errUsage},
		{name: "empty config", args: []string{"-config", ""}, wantErr: errUsage},
		{name: "old command form", args: []string{"--", "server"}, wantErr: errUsage},
		{name: "extra arguments", args: []string{"-config", "g.yaml", "server"}, wantErr: errUsage},
		{name: "zero timeout", args: []string{"-config", "g.yaml", "-call-timeout", "0s"}, wantErr: errUsage},
		{name: "negative timeout", args: []string{"-config", "g.yaml", "-call-timeout", "-1s"}, wantErr: errUsage},
		{name: "bad timeout", args: []string{"-config", "g.yaml", "-call-timeout", "soon"}, wantErr: errUsage},
		{name: "bad log level", args: []string{"-config", "g.yaml", "-log-level", "loud"}, wantErr: errUsage},
		{name: "unknown flag", args: []string{"-nope", "-config", "g.yaml"}, wantErr: errUsage},
		{name: "help", args: []string{"-h"}, wantErr: flag.ErrHelp},
		{name: "http on 127.0.0.1", args: []string{"-config", "g.yaml", "-http", "127.0.0.1:8080"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "127.0.0.1:8080"},
		{name: "http on ::1", args: []string{"-config", "g.yaml", "-http", "[::1]:0"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "[::1]:0"},
		{name: "http on localhost", args: []string{"-config", "g.yaml", "-http", "localhost:0"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "localhost:0"},
		{name: "http on 127.0.0.2", args: []string{"-config", "g.yaml", "-http", "127.0.0.2:80"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "127.0.0.2:80"},
		{name: "origins with http", args: []string{"-config", "g.yaml", "-http", "127.0.0.1:0", "-allow-origin", "http://a", "-allow-origin", "http://b"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "127.0.0.1:0", wantOrigins: "http://a,http://b"},
		{name: "http on all interfaces", args: []string{"-config", "g.yaml", "-http", ":8080"}, wantErr: errUsage},
		{name: "http on 0.0.0.0", args: []string{"-config", "g.yaml", "-http", "0.0.0.0:8080"}, wantErr: errUsage},
		{name: "http on [::]", args: []string{"-config", "g.yaml", "-http", "[::]:8080"}, wantErr: errUsage},
		{name: "http on a LAN address", args: []string{"-config", "g.yaml", "-http", "192.168.1.10:8080"}, wantErr: errUsage},
		{name: "http on a hostname", args: []string{"-config", "g.yaml", "-http", "example.com:8080"}, wantErr: errUsage},
		{name: "http without port", args: []string{"-config", "g.yaml", "-http", "127.0.0.1"}, wantErr: errUsage},
		{name: "origins without http", args: []string{"-config", "g.yaml", "-allow-origin", "http://a"}, wantErr: errUsage},
		{name: "all interfaces with TLS", args: []string{"-config", "g.yaml", "-http", "0.0.0.0:8443", "-tls-cert", "c.pem", "-tls-key", "k.pem"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "0.0.0.0:8443"},
		{name: "LAN address with TLS", args: []string{"-config", "g.yaml", "-http", "192.168.1.10:8443", "-tls-cert", "c.pem", "-tls-key", "k.pem"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "192.168.1.10:8443"},
		{name: "all interfaces behind a TLS proxy", args: []string{"-config", "g.yaml", "-http", ":8080", "-behind-tls-proxy"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: ":8080"},
		{name: "loopback with TLS", args: []string{"-config", "g.yaml", "-http", "127.0.0.1:8443", "-tls-cert", "c.pem", "-tls-key", "k.pem"}, wantConfig: "g.yaml", wantTimeout: 60 * time.Second, wantHTTP: "127.0.0.1:8443"},
		{name: "cert without key", args: []string{"-config", "g.yaml", "-http", "0.0.0.0:8443", "-tls-cert", "c.pem"}, wantErr: errUsage},
		{name: "key without cert", args: []string{"-config", "g.yaml", "-http", "0.0.0.0:8443", "-tls-key", "k.pem"}, wantErr: errUsage},
		{name: "TLS and proxy together", args: []string{"-config", "g.yaml", "-http", "0.0.0.0:8443", "-tls-cert", "c.pem", "-tls-key", "k.pem", "-behind-tls-proxy"}, wantErr: errUsage},
		{name: "TLS without http", args: []string{"-config", "g.yaml", "-tls-cert", "c.pem", "-tls-key", "k.pem"}, wantErr: errUsage},
		{name: "proxy flag without http", args: []string{"-config", "g.yaml", "-behind-tls-proxy"}, wantErr: errUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseFlags(tt.args, io.Discard)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parseFlags() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags() error = %v", err)
			}
			if f.configPath != tt.wantConfig {
				t.Errorf("configPath = %q, want %q", f.configPath, tt.wantConfig)
			}
			if f.callTimeout != tt.wantTimeout {
				t.Errorf("callTimeout = %v, want %v", f.callTimeout, tt.wantTimeout)
			}
			if f.httpAddr != tt.wantHTTP || f.allowedOrigins.String() != tt.wantOrigins {
				t.Errorf("http = %q origins = %q, want %q and %q", f.httpAddr, f.allowedOrigins.String(), tt.wantHTTP, tt.wantOrigins)
			}
		})
	}
}
