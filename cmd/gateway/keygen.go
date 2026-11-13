package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/config"
)

// apiKeyEnv holds a stdio client's API key. The MCP spec says stdio servers
// take credentials from the environment, and client configs (such as
// Claude Desktop's) can set environment variables per server.
const apiKeyEnv = "MCP_GATEWAY_API_KEY"

// runKeygen creates a key for one agent. The key goes to stdout alone, so
// scripts can capture it; the config lines with its hash go to stderr as
// YAML that can be pasted under agents:. The key is never written anywhere
// else, so it cannot be shown again.
func runKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gateway keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gateway keygen -agent NAME")
		fs.PrintDefaults()
	}
	agent := fs.String("agent", "", "name of the agent the key is for (required)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return usageFailure(fs, "unexpected arguments")
	}
	if *agent == "" {
		return usageFailure(fs, "missing -agent")
	}
	if err := config.CheckName(*agent); err != nil {
		return usageFailure(fs, "-agent: "+err.Error())
	}

	key, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, key); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	_, err = fmt.Fprintf(stderr, "# The API key for agent %q is on stdout. It is shown only once.\n"+
		"# Give it to the agent (stdio: %s; HTTP: Authorization: Bearer <key>)\n"+
		"# and add these lines under agents: in the config:\n"+
		"  - name: %s\n"+
		"    key_sha256: %s\n",
		*agent, apiKeyEnv, *agent, auth.HashKey(key))
	return err
}
