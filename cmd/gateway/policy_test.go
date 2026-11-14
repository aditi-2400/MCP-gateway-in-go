package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// echoOnlyPolicy lets the tester call alpha__echo with short text, and
// nothing else.
const echoOnlyPolicy = `
roles:
  echoer:
    allow:
      - tool: alpha__echo
        args:
          text: { max_length: 10 }
agents:
  tester: [echoer]
`

// writePolicy replaces the policy writeConfig put next to cfg.
func writePolicy(t *testing.T, cfg, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "policy.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func callParams(name, args string) string {
	return `{"name":"` + name + `","arguments":` + args + `,` + validMeta + `}`
}

// wantDeniedArgs checks for a tool result with isError and the given text.
func wantDeniedArgs(t *testing.T, resp *jsonrpc.Message, text string) {
	t.Helper()
	var r struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if resp.Error != nil || json.Unmarshal(resp.Result, &r) != nil || !r.IsError || len(r.Content) != 1 || r.Content[0].Text != text {
		t.Fatalf("response = result %s, error %v; want isError text %q", resp.Result, resp.Error, text)
	}
}

func TestRunStdioEnforcesPolicy(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, echoOnlyPolicy)
	g := startGateway(t, context.Background(), "-config", cfg)

	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	resp := g.next()
	if resp.Error != nil || strings.Join(toolNamesOf(t, resp.Result), ",") != "alpha__echo" {
		t.Fatalf("tools/list = %s, %v; want only alpha__echo", resp.Result, resp.Error)
	}

	g.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` + callParams("alpha__echo", `{"text":"hi"}`) + `}`)
	if resp := g.next(); resp.Error != nil || !strings.Contains(string(resp.Result), `"text":"hi"`) {
		t.Fatalf("allowed call = %s, %v", resp.Result, resp.Error)
	}

	g.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":` + callParams("alpha__add", `{"a":1,"b":2}`) + `}`)
	wantError(t, g.next(), ptr(jsonrpc.NewIntID(3)), jsonrpc.CodeInvalidParams)

	g.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":` + callParams("alpha__echo", `{"text":"far too long for the rule"}`) + `}`)
	wantDeniedArgs(t, g.next(), `Denied by gateway policy: argument "text" is longer than 10 bytes`)

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	logs := g.stderr.String()
	for _, want := range []string{`msg="call denied" component=policy agent=tester tool=alpha__add`, `tool=alpha__echo reason=`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs do not contain %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "far too long") {
		t.Error("logs contain a denied argument value")
	}
}

func TestRunHTTPEnforcesPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, echoOnlyPolicy)
	g := startGateway(t, ctx, "-config", cfg, "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	status, resp := postMCP(t, url, "tools/list", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+validMeta+`}}`)
	if status != http.StatusOK || strings.Join(toolNamesOf(t, resp.Result), ",") != "alpha__echo" {
		t.Fatalf("tools/list = %d %s; want only alpha__echo", status, resp.Result)
	}

	status, resp = postMCP(t, url, "tools/call", "alpha__add", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+callParams("alpha__add", `{"a":1,"b":2}`)+`}`)
	if status != http.StatusOK || resp.Error == nil || resp.Error.Message != "Unknown tool: alpha__add" {
		t.Fatalf("hidden tool = %d %+v; want Unknown tool", status, resp.Error)
	}

	status, resp = postMCP(t, url, "tools/call", "alpha__echo", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+callParams("alpha__echo", `{"text":"far too long for the rule"}`)+`}`)
	if status != http.StatusOK {
		t.Fatalf("denied args status = %d, want 200", status)
	}
	wantDeniedArgs(t, resp, `Denied by gateway policy: argument "text" is longer than 10 bytes`)

	// alpha__lookup is hidden, so its Mcp-Param headers are never checked:
	// a wrong header gets the same "Unknown tool" as no header. Otherwise
	// the header error would show that the hidden tool exists.
	for _, headers := range []map[string]string{nil, {"Mcp-Param-Region": "wrong"}} {
		status, resp := postLookup(t, url, headers)
		if status != http.StatusOK || resp.Error == nil || resp.Error.Message != "Unknown tool: alpha__lookup" {
			t.Errorf("hidden lookup with %v = %d %+v; want Unknown tool", headers, status, resp.Error)
		}
	}
}

func TestRunRefusesBadPolicyBeforeStartingAnything(t *testing.T) {
	tests := []struct {
		name    string
		policy  string // empty means no file
		wantErr string
	}{
		{name: "missing file", wantErr: "policy"},
		{name: "invalid policy", policy: "roles: {}\n", wantErr: "no agents"},
		{name: "unknown upstream", policy: "roles:\n  r:\n    allow:\n      - tool: alhpa__echo\nagents:\n  tester: [r]\n", wantErr: `upstream "alhpa"`},
		{name: "agent without a key", policy: "roles:\n  r:\n    allow:\n      - tool: alpha__echo\nagents:\n  ghost: [r]\n", wantErr: `"ghost"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fakeConfig(t, "alpha")
			policyPath := filepath.Join(filepath.Dir(cfg), "policy.yaml")
			if tt.policy == "" {
				if err := os.Remove(policyPath); err != nil {
					t.Fatal(err)
				}
			} else {
				writePolicy(t, cfg, tt.policy)
			}
			stderr := &syncBuffer{}
			err := run(context.Background(), withPolicy([]string{"-config", cfg}), strings.NewReader(""), io.Discard, stderr, testEnv)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), policyPath) {
				t.Fatalf("run() error = %v, want one naming %s and mentioning %q", err, policyPath, tt.wantErr)
			}
			if strings.Contains(stderr.String(), "upstream started") {
				t.Error("an upstream started before the policy was checked")
			}
		})
	}
}

func TestBinaryWithoutPolicyExits2(t *testing.T) {
	g := startGatewayProcess(t, "-config", fakeConfig(t, "alpha"), "-policy", "")
	if code := g.exitCode(); code != 2 {
		t.Fatalf("exit code = %d, want 2\nstderr:\n%s", code, g.stderr)
	}
	if !strings.Contains(g.stderr.String(), "missing -policy") {
		t.Errorf("stderr = %q, want the usage problem", g.stderr)
	}
}

func toolNamesOf(t *testing.T, result json.RawMessage) []string {
	t.Helper()
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("decode tools/list result %s: %v", result, err)
	}
	names := make([]string, len(r.Tools))
	for i, tool := range r.Tools {
		names[i] = tool.Name
	}
	return names
}
