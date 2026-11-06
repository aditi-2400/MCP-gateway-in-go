package main

// End-to-end tests: the real gateway binary runs as its own process in front
// of the real fake upstream binary, the way a local MCP client would start it.

import (
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/mcp"
)

// gatewayProcess is a running gateway binary with pipes to its stdio.
type gatewayProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *jsonrpc.Reader
	stderr *syncBuffer
	exited chan error
}

func startGatewayProcess(t *testing.T, args ...string) *gatewayProcess {
	t.Helper()
	cmd := exec.Command(gatewayBin, args...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}

	g := &gatewayProcess{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		out:    jsonrpc.NewReader(stdout, jsonrpc.DefaultMaxMessageSize),
		stderr: stderr,
		exited: make(chan error, 1),
	}
	t.Cleanup(func() {
		// Only matters if a test failed early and left the process running.
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	})
	return g
}

func (g *gatewayProcess) send(line string) {
	g.t.Helper()
	if _, err := io.WriteString(g.stdin, line+"\n"); err != nil {
		g.t.Fatalf("write to gateway: %v", err)
	}
}

// next reads one line from the gateway's stdout. Any line that is not a
// valid JSON-RPC message fails the test, because MCP's stdio transport
// allows nothing else on stdout.
func (g *gatewayProcess) next() *jsonrpc.Message {
	g.t.Helper()
	type result struct {
		msg *jsonrpc.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := g.out.Read()
		ch <- result{msg, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			g.t.Fatalf("read from gateway stdout: %v\nstderr:\n%s", r.err, g.stderr)
		}
		return r.msg
	case <-time.After(testTimeout):
		g.t.Fatalf("no message from gateway in time\nstderr:\n%s", g.stderr)
		return nil
	}
}

// exitCode waits for the gateway to exit and returns its exit status.
func (g *gatewayProcess) exitCode() int {
	g.t.Helper()
	go func() {
		// Wait must only run after stdout reads are done; callers ensure that.
		g.exited <- g.cmd.Wait()
	}()
	select {
	case err := <-g.exited:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			g.t.Fatalf("wait for gateway: %v", err)
		}
		return 0
	case <-time.After(testTimeout):
		g.t.Fatalf("gateway did not exit in time\nstderr:\n%s", g.stderr)
		return -1
	}
}

func TestBinaryEndToEnd(t *testing.T) {
	// Two copies of the fake server have the same tool names, so every name
	// collides unless the gateway prefixes them.
	g := startGatewayProcess(t, "-config", fakeConfig(t, "alpha", "beta"))

	// Requests go one at a time, because the gateway may answer concurrent
	// requests in any order.
	steps := []struct {
		name  string
		send  string
		check func(t *testing.T, resp *jsonrpc.Message)
	}{
		{
			name: "server/discover is answered by the gateway",
			send: `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantResult(t, resp, jsonrpc.NewIntID(1), `"supportedVersions":["2026-07-28"]`)
				wantResult(t, resp, jsonrpc.NewIntID(1), `"name":"mcp-gateway"`)
			},
		},
		{
			name: "tools/list merges both upstreams with prefixes",
			send: `{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				for _, tool := range []string{"echo", "add", "fail"} {
					for _, up := range []string{"alpha", "beta"} {
						wantResult(t, resp, jsonrpc.NewStringID("list"), `"name":"`+up+`__`+tool+`"`)
					}
				}
			},
		},
		{
			name: "tools/call routes by prefix",
			send: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"alpha__add","arguments":{"a":2,"b":3},` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantResult(t, resp, jsonrpc.NewIntID(3), `"structuredContent":{"sum":5}`)
			},
		},
		{
			name: "tool error is a result with isError",
			send: `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"beta__fail","arguments":{},` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantResult(t, resp, jsonrpc.NewIntID(4), `"isError":true`)
			},
		},
		{
			name: "unknown tool on a known upstream is a protocol error",
			send: `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"alpha__nope","arguments":{},` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantError(t, resp, ptr(jsonrpc.NewIntID(5)), jsonrpc.CodeInvalidParams)
			},
		},
		{
			name: "unknown upstream prefix is a protocol error",
			send: `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"gamma__echo","arguments":{},` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantError(t, resp, ptr(jsonrpc.NewIntID(6)), jsonrpc.CodeInvalidParams)
			},
		},
		{
			name: "unsupported protocol version is rejected by the gateway",
			send: `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantError(t, resp, ptr(jsonrpc.NewIntID(7)), mcp.CodeUnsupportedProtocolVersion)
				if !strings.Contains(string(resp.Error.Data), `"requested":"2025-06-18"`) {
					t.Errorf("error data = %s, want the requested version", resp.Error.Data)
				}
			},
		},
		{
			name: "bad JSON gets a parse error with null id",
			send: `{"jsonrpc":"2.0","id":8,`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantError(t, resp, nil, jsonrpc.CodeParseError)
			},
		},
		{
			// Notifications get no reply, so the next message must be the
			// answer to the request sent after it.
			name: "notification gets no reply",
			send: `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n" +
				`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"beta__echo","arguments":{"text":"still here"},` + validMeta + `}}`,
			check: func(t *testing.T, resp *jsonrpc.Message) {
				wantResult(t, resp, jsonrpc.NewIntID(9), `"text":"still here"`)
			},
		},
	}
	for _, step := range steps {
		g.send(step.send)
		resp := g.next()
		t.Run(step.name, func(t *testing.T) { step.check(t, resp) })
	}

	g.stdin.Close()
	if _, err := g.out.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("after stdin closed, stdout read = %v, want EOF (no extra output)", err)
	}
	if code := g.exitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, g.stderr)
	}
}

func TestBinaryExitCodes(t *testing.T) {
	skipWithoutShell(t)

	t.Run("usage error exits 2", func(t *testing.T) {
		g := startGatewayProcess(t)
		g.stdin.Close()
		if code := g.exitCode(); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if !strings.Contains(g.stderr.String(), "missing -config") {
			t.Errorf("stderr = %q, want the usage problem", g.stderr)
		}
	})

	t.Run("failed upstream start exits 1", func(t *testing.T) {
		cfg := writeConfig(t, upstreamEntry{name: "broken", command: filepath.Join(t.TempDir(), "missing")})
		g := startGatewayProcess(t, "-config", cfg)
		if code := g.exitCode(); code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", code, g.stderr)
		}
		if !strings.Contains(g.stderr.String(), `"broken"`) {
			t.Errorf("stderr = %q, want the failed upstream named", g.stderr)
		}
	})

	t.Run("upstream exit exits 1", func(t *testing.T) {
		g := startGatewayProcess(t, "-config", writeConfig(t, discoverThenExit))
		if code := g.exitCode(); code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", code, g.stderr)
		}
	})

	t.Run("SIGTERM exits 0", func(t *testing.T) {
		g := startGatewayProcess(t, "-config", fakeConfig(t, "fake"))
		g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
		g.next()

		if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send SIGTERM: %v", err)
		}
		if code := g.exitCode(); code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, g.stderr)
		}
		if !strings.Contains(g.stderr.String(), "gateway stopped") {
			t.Errorf("stderr = %q, want a clean stop", g.stderr)
		}
	})
}

func wantResult(t *testing.T, resp *jsonrpc.Message, id jsonrpc.ID, contains string) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("error = %v, want a result", resp.Error)
	}
	if resp.ID == nil || *resp.ID != id {
		t.Fatalf("id = %v, want %v", resp.ID, id)
	}
	if !strings.Contains(string(resp.Result), contains) {
		t.Fatalf("result = %s, want it to contain %s", resp.Result, contains)
	}
}

func wantError(t *testing.T, resp *jsonrpc.Message, id *jsonrpc.ID, code int) {
	t.Helper()
	if resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("response = result %s, error %v, want error code %d", resp.Result, resp.Error, code)
	}
	if (id == nil) != (resp.ID == nil) || (id != nil && *resp.ID != *id) {
		t.Fatalf("id = %v, want %v", resp.ID, id)
	}
}

func ptr[T any](v T) *T { return &v }
