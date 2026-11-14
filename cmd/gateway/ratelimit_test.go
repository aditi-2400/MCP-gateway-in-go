package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/ratelimit"
)

// burstPolicy allows alpha__add twice an hour and alpha__echo (short text
// only) twice an hour, so the third call of each is limited.
const burstPolicy = `
roles:
  r:
    allow:
      - tool: alpha__add
      - tool: alpha__echo
        args:
          text: { max_length: 5 }
agents:
  tester: [r]
limits:
  default: { rate: 1000/second, burst: 1000 }
  tools:
    - { tool: alpha__add, rate: 1/hour, burst: 2 }
    - { tool: alpha__echo, rate: 1/hour, burst: 2 }
`

func addRequest(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":` + callParams("alpha__add", `{"a":1,"b":2}`) + `}`
}

// wantRateLimited checks for a -31029 error with a positive retryAfterMs.
func wantRateLimited(t *testing.T, resp *jsonrpc.Message, tool string) {
	t.Helper()
	if resp.Error == nil || resp.Error.Code != ratelimit.CodeRateLimited || resp.Error.Message != "Rate limit exceeded for tool "+tool {
		t.Fatalf("response = result %s, error %+v; want a rate limit error for %s", resp.Result, resp.Error, tool)
	}
	var data struct {
		RetryAfterMs int64 `json:"retryAfterMs"`
	}
	if err := json.Unmarshal(resp.Error.Data, &data); err != nil || data.RetryAfterMs <= 0 {
		t.Errorf("data = %s, want a positive retryAfterMs", resp.Error.Data)
	}
}

func TestRunStdioRateLimits(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, burstPolicy)
	g := startGateway(t, context.Background(), "-config", cfg)

	for id := 1; id <= 2; id++ {
		g.send(addRequest(id))
		if resp := g.next(); resp.Error != nil {
			t.Fatalf("call %d: %v, want allowed (burst 2)", id, resp.Error)
		}
	}
	g.send(addRequest(3))
	wantRateLimited(t, g.next(), "alpha__add")

	// Calls the policy denies never reach the limiter, so three echoes with
	// text that is too long leave both echo tokens in place.
	for id := 10; id < 13; id++ {
		g.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":` + callParams("alpha__echo", `{"text":"much too long"}`) + `}`)
		wantDeniedArgs(t, g.next(), `Denied by gateway policy: argument "text" is longer than 5 bytes`)
	}
	for id := 20; id < 22; id++ {
		g.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":` + callParams("alpha__echo", `{"text":"hi"}`) + `}`)
		if resp := g.next(); resp.Error != nil {
			t.Fatalf("echo %d: %v, want allowed; denied calls must not use tokens", id, resp.Error)
		}
	}

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if logs := g.stderr.String(); !strings.Contains(logs, `msg="call rate limited" component=ratelimit agent=tester tool=alpha__add`) {
		t.Errorf("logs have no rate limit line:\n%s", logs)
	}
}

func TestRunHTTPRateLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, burstPolicy)
	g := startGateway(t, ctx, "-config", cfg, "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	post := func(id int) *http.Response {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(addRequest(id)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "alpha__add")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	for id := 1; id <= 2; id++ {
		if resp := post(id); resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d: status %d, want 200", id, resp.StatusCode)
		}
	}
	resp := post(3)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third call: status %d, want 429", resp.StatusCode)
	}
	// 1/hour: the next token is close to an hour away.
	secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || secs < 3500 || secs > 3600 {
		t.Errorf("Retry-After = %q, want about 3600 seconds", resp.Header.Get("Retry-After"))
	}
	var msg jsonrpc.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	wantRateLimited(t, &msg, "alpha__add")
}
