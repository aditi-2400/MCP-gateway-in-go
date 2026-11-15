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

// CallParams returns the params of a tools/call to the add tool, named tool
// as the target knows it, the way a 2026-07-28 client sends them.
func CallParams(tool string) json.RawMessage {
	return json.RawMessage(`{"name":"` + tool + `","arguments":{"a":1,"b":2},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`)
}

// StdioTarget is a server to measure, started as a child process that talks
// over stdin and stdout.
type StdioTarget struct {
	Name    string           // for results, e.g. "direct" or "gateway"
	Command upstream.Command // how to start it
	Tool    string           // the add tool's name as this target knows it
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
// gateway's arguments. Agent's key must hash to keyHash.
//
// The policy is the smallest one that allows the calls: every tool of the
// upstream, with a limit too high to reach.
func WriteGatewayFiles(dir, upstreamPath, keyHash string) ([]string, error) {
	config := fmt.Sprintf("upstreams:\n  - name: %s\n    command: %q\nagents:\n  - name: %s\n    key_sha256: %s\n",
		UpstreamName, upstreamPath, Agent, keyHash)
	policy := fmt.Sprintf("roles:\n  all:\n    allow:\n      - tool: %s__*\nagents:\n  %s: [all]\n"+
		"limits:\n  default: { rate: 1000000/second, burst: 1000000 }\n", UpstreamName, Agent)

	configPath := filepath.Join(dir, "gateway.yaml")
	policyPath := filepath.Join(dir, "policy.yaml")
	for path, text := range map[string]string{configPath: config, policyPath: policy} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return []string{"-config", configPath, "-policy", policyPath, "-audit-db", filepath.Join(dir, "audit.db")}, nil
}
