package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/config"
)

func envWithKey(key string) func(string) string {
	return func(name string) string {
		if name == apiKeyEnv {
			return key
		}
		return ""
	}
}

func TestRunStdioNeedsAValidKeyBeforeStartingUpstreams(t *testing.T) {
	// The only upstream can't start. If auth ran after startup, the error
	// would be about the missing program.
	cfg := writeConfig(t, upstreamEntry{name: "never", command: filepath.Join(t.TempDir(), "missing")})
	otherKey, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		key     string
		wantErr error
	}{
		{name: "no key", key: "", wantErr: auth.ErrNoKey},
		{name: "unknown key", key: otherKey, wantErr: auth.ErrInvalidKey},
		{name: "not a key", key: "hunter2", wantErr: auth.ErrInvalidKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := run(context.Background(), []string{"-config", cfg}, strings.NewReader(""), io.Discard, &stderr, envWithKey(tt.key))
			if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), apiKeyEnv) {
				t.Fatalf("run() error = %v, want %v naming %s", err, tt.wantErr, apiKeyEnv)
			}
			if strings.Contains(stderr.String(), "upstream started") {
				t.Error("an upstream was started before the key was checked")
			}
			if tt.key != "" && strings.Contains(err.Error()+stderr.String(), tt.key) {
				t.Error("the rejected key appears in the error or logs")
			}
		})
	}
}

func TestRunStdioLogsTheAgentNotTheKey(t *testing.T) {
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"))
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	logs := g.stderr.String()
	if !strings.Contains(logs, "agent=tester") {
		t.Errorf("logs = %q, want the agent named", logs)
	}
	if strings.Contains(logs, testAPIKey) || strings.Contains(logs, auth.HashKey(testAPIKey)) {
		t.Fatal("logs contain the key or its hash")
	}
}

func TestRunHTTPNeedsAKeyOnEveryRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// No stdio key in HTTP mode: each request brings its own.
	g := startGateway(t, ctx, "-config", fakeConfig(t, "fake"), "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`

	for name, header := range map[string]string{"no header": "", "wrong key": "Bearer " + auth.HashKey("x")} {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/list")
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
	if status, _ := postMCP(t, url, "tools/list", "", body); status != http.StatusOK {
		t.Fatalf("with the key: status = %d, want 200", status)
	}
}

func TestRunHTTPIgnoresTheStdioKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &gateway{t: t, done: make(chan error, 1), stderr: &syncBuffer{}}
	go func() {
		g.done <- run(ctx, []string{"-config", fakeConfig(t, "fake"), "-http", "127.0.0.1:0"}, strings.NewReader(""), io.Discard, g.stderr, envWithKey(""))
	}()
	url := waitForHTTP(t, g.stderr)
	if status, _ := postMCP(t, url, "tools/list", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+validMeta+`}}`); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 without %s set", status, apiKeyEnv)
	}
	cancel()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
}

func TestKeygen(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"keygen", "-agent", "ci-bot"}, nil, &stdout, &stderr, testEnv); err != nil {
		t.Fatalf("keygen error = %v", err)
	}
	key := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(key, auth.KeyPrefix) || strings.Contains(key, "\n") {
		t.Fatalf("stdout = %q, want exactly one key", stdout.String())
	}
	if strings.Contains(stderr.String(), key) {
		t.Error("the key also appears on stderr")
	}

	// The stderr lines must work pasted under agents: as they are.
	cfgText := "upstreams:\n  - name: fake\n    command: x\nagents:\n" + stderr.String()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config from keygen output does not load: %v\n%s", err, cfgText)
	}
	authn, err := auth.New(agentEntries(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if agent, err := authn.Authenticate(key); err != nil || agent.Name != "ci-bot" {
		t.Fatalf("Authenticate(new key) = %+v, %v, want ci-bot", agent, err)
	}
}

func TestKeygenUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr error
	}{
		{name: "missing agent", args: []string{"keygen"}, wantErr: errUsage},
		{name: "bad agent name", args: []string{"keygen", "-agent", "CI_Bot"}, wantErr: errUsage},
		{name: "extra argument", args: []string{"keygen", "-agent", "a", "more"}, wantErr: errUsage},
		{name: "unknown flag", args: []string{"keygen", "-nope"}, wantErr: errUsage},
		{name: "help", args: []string{"keygen", "-h"}, wantErr: flag.ErrHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := run(context.Background(), tt.args, nil, &stdout, io.Discard, testEnv)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want no key printed", stdout.String())
			}
		})
	}
}

func TestBinaryKeygenThenConnect(t *testing.T) {
	keygen := exec.Command(gatewayBin, "keygen", "-agent", "desktop")
	var keyOut, snippet bytes.Buffer
	keygen.Stdout, keygen.Stderr = &keyOut, &snippet
	if err := keygen.Run(); err != nil {
		t.Fatalf("keygen: %v\n%s", err, snippet.String())
	}
	key := strings.TrimSpace(keyOut.String())

	cfgText := "upstreams:\n  - name: fake\n    command: " + fakeUpstreamBin + "\nagents:\n" + snippet.String()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}

	gw := exec.Command(gatewayBin, "-config", path)
	gw.Env = append(os.Environ(), apiKeyEnv+"="+key)
	gw.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}` + "\n")
	var out, logs bytes.Buffer
	gw.Stdout, gw.Stderr = &out, &logs
	if err := gw.Run(); err != nil {
		t.Fatalf("gateway: %v\n%s", err, logs.String())
	}
	if !strings.Contains(out.String(), `"name":"fake__echo"`) {
		t.Fatalf("stdout = %s, want the tool list", out.String())
	}
	if !strings.Contains(logs.String(), "agent=desktop") {
		t.Errorf("logs = %s, want agent=desktop", logs.String())
	}
}

func TestBinaryStdioWithoutKeyExits1(t *testing.T) {
	gw := exec.Command(gatewayBin, "-config", fakeConfig(t, "fake"))
	gw.Env = append(os.Environ(), apiKeyEnv+"=")
	var logs bytes.Buffer
	gw.Stderr = &logs
	err := gw.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want code 1\n%s", err, logs.String())
	}
	if !strings.Contains(logs.String(), apiKeyEnv) {
		t.Errorf("stderr = %q, want it to name %s", logs.String(), apiKeyEnv)
	}
}
