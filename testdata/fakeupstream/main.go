// Command fakeupstream runs the fake MCP server over stdin and stdout.
// Tests build it with: go build ./testdata/fakeupstream
package main

import (
	"fmt"
	"os"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/fakeupstream"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

func main() {
	// Serve returns when stdin is closed, which is how MCP stdio clients
	// ask a server to shut down.
	if err := fakeupstream.Serve(os.Stdin, os.Stdout, jsonrpc.DefaultMaxMessageSize); err != nil {
		// stdout is reserved for protocol messages, so errors go to stderr.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
