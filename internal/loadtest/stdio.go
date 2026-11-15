package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

// Agent is the agent name the benchmark gateway config gives the key.
const Agent = "bench"

// KeyEnv is the environment variable the gateway reads its stdio API key
// from (apiKeyEnv in cmd/gateway).
const KeyEnv = "MCP_GATEWAY_API_KEY"

// UpstreamName is what the benchmark gateway config calls the upstream, so
// its tools are reached as UpstreamName + "__" + tool.
const UpstreamName = "fake"

// Tool is the fake upstream tool every benchmark call uses. echo takes a
// string, so the realistic policy's path and pattern checks have something
// to check.
const Tool = "echo"

// Text is the argument sent to Tool: a path inside the folder the realistic
// policy allows.
const Text = "/tmp/agent-workspace/notes/today.txt"

// CallParams returns the params of a tools/call to Tool, named tool as the
// target knows it, the way a 2026-07-28 client sends them.
func CallParams(tool string) json.RawMessage {
	return json.RawMessage(`{"name":"` + tool + `","arguments":{"text":"` + Text + `"},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`)
}

// StdioTarget is a server to measure, started as a child process that talks
// over stdin and stdout.
type StdioTarget struct {
	Name    string           // for results, e.g. "direct" or "gateway"
	Command upstream.Command // how to start it
	Tool    string           // Tool's name as this target knows it
}

// MeasureStdio starts target, runs Sequential against it, and stops it.
// The child's stderr goes to stderr. Like every upstream.Start child, it
// inherits this process's environment.
func MeasureStdio(ctx context.Context, target StdioTarget, warmup, n int, stderr io.Writer) (times []time.Duration, err error) {
	proc, err := upstream.Start(target.Command, upstream.Options{Stderr: stderr})
	if err != nil {
		return nil, fmt.Errorf("%s: start: %w", target.Name, err)
	}
	defer func() {
		if closeErr := proc.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("%s: stop: %w", target.Name, closeErr)
		}
	}()
	times, err = Sequential(ctx, proc, CallParams(target.Tool), warmup, n)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target.Name, err)
	}
	return times, nil
}

// WriteGatewayFiles writes a gateway config and policy into dir for a
// gateway in front of the upstream at upstreamPath, and returns the
// gateway's arguments. Agent's key must hash to keyHash. policyName is one
// of Policies.
func WriteGatewayFiles(dir, upstreamPath, keyHash, policyName string) ([]string, error) {
	policy, ok := policies[policyName]
	if !ok {
		return nil, fmt.Errorf("unknown policy %q, want one of %v", policyName, Policies)
	}
	config := fmt.Sprintf("upstreams:\n  - name: %s\n    command: %q\nagents:\n  - name: %s\n    key_sha256: %s\n",
		UpstreamName, upstreamPath, Agent, keyHash)

	configPath := filepath.Join(dir, "gateway.yaml")
	policyPath := filepath.Join(dir, "policy.yaml")
	for path, text := range map[string]string{configPath: config, policyPath: policy} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return []string{"-config", configPath, "-policy", policyPath, "-audit-db", filepath.Join(dir, "audit.db")}, nil
}
