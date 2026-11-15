package loadtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// Caller sends one request and waits for its reply. upstream.Conn is one.
type Caller interface {
	Call(ctx context.Context, method string, params json.RawMessage) (*jsonrpc.Message, error)
}

// Sequential makes warmup calls to tools/call with params, then n more, one
// at a time, and returns how long each of the n took.
//
// Warm-up calls are not timed: the first calls pay for things a running
// gateway has already done, like loading code and filling caches.
//
// Any call that fails stops the run with an error: no reply, an error reply,
// or a tool result with isError. A benchmark that mixed fast failures in
// with real calls would report numbers no real call had.
func Sequential(ctx context.Context, c Caller, params json.RawMessage, warmup, n int) ([]time.Duration, error) {
	if warmup < 0 || n <= 0 {
		return nil, fmt.Errorf("loadtest: need warmup >= 0 and n > 0, got %d and %d", warmup, n)
	}
	for i := range warmup {
		if err := callOnce(ctx, c, params); err != nil {
			return nil, fmt.Errorf("warm-up call %d: %w", i+1, err)
		}
	}
	times := make([]time.Duration, 0, n)
	for i := range n {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := time.Now()
		resp, err := c.Call(ctx, "tools/call", params)
		took := time.Since(start)
		// Checked after the clock stops, so checking costs nothing measured.
		if err := checkReply(resp, err); err != nil {
			return nil, fmt.Errorf("call %d: %w", i+1, err)
		}
		times = append(times, took)
	}
	return times, nil
}

func callOnce(ctx context.Context, c Caller, params json.RawMessage) error {
	resp, err := c.Call(ctx, "tools/call", params)
	return checkReply(resp, err)
}

// checkReply returns an error unless resp is a successful tool result.
func checkReply(resp *jsonrpc.Message, err error) error {
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("error reply %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("unreadable result: %w", err)
	}
	if result.IsError {
		return errors.New("tool error (isError)")
	}
	return nil
}
