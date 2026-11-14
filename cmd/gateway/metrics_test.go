package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var metricsLine = regexp.MustCompile(`msg="metrics listening" .*?addr=(\S+)`)

// waitForMetrics waits for the gateway to log its metrics address and
// returns the base URL.
func waitForMetrics(t *testing.T, stderr *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if m := metricsLine.FindStringSubmatch(stderr.String()); m != nil {
			return "http://" + m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gateway did not log a metrics address\nstderr:\n%s", stderr)
	return ""
}

// scrape fetches url and returns the status and body.
func scrape(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestRunServesMetrics(t *testing.T) {
	cfg := fakeConfig(t, "alpha")
	writePolicy(t, cfg, auditPolicy)
	g := startGateway(t, context.Background(), "-config", cfg, "-metrics", "127.0.0.1:0")
	base := waitForMetrics(t, g.stderr)

	// Listing tools first lets the router learn the real tool names.
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()
	calls := []struct{ tool, args string }{
		{"alpha__add", `{"a":1,"b":2}`},                               // allowed
		{"alpha__add", `{"a":1,"b":2}`},                               // rate limited
		{"alpha__echo", `{"text":"` + strings.Repeat("x", 41) + `"}`}, // arguments denied
		{"alpha__count", `{"n":1}`},                                   // tool denied
		{"alpha__made_up", `{}`},                                      // unknown tool
	}
	for i, c := range calls {
		g.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i+2) + `,"method":"tools/call","params":` + callParams(c.tool, c.args) + `}`)
		g.next()
	}

	status, text := scrape(t, http.MethodGet, base+"/metrics")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", status)
	}
	for _, want := range []string{
		`mcpgw_tool_calls_total{agent="tester",decision="allowed",status="ok",tool="alpha__add"} 1`,
		`mcpgw_tool_call_denials_total{agent="tester",reason="rate_limited",tool="alpha__add"} 1`,
		`mcpgw_tool_call_denials_total{agent="tester",reason="arguments",tool="alpha__echo"} 1`,
		`mcpgw_tool_call_denials_total{agent="tester",reason="tool",tool="alpha__count"} 1`,
		`mcpgw_tool_call_denials_total{agent="tester",reason="tool",tool="other"} 1`,
		`mcpgw_upstream_latency_seconds_count{tool="alpha__add"} 1`,
		`mcpgw_tool_call_duration_seconds_count{decision="denied"} 3`,
		`mcpgw_audit_queue_slots 1024`,
		`mcpgw_audit_refused_total 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics have no %q", want)
		}
	}
	if strings.Contains(text, "alpha__made_up") {
		t.Error("an unknown tool name became a label")
	}

	// Only GET /metrics is served.
	if status, _ := scrape(t, http.MethodGet, base+"/mcp"); status != http.StatusNotFound {
		t.Errorf("GET /mcp on the metrics address = %d, want 404", status)
	}
	if status, _ := scrape(t, http.MethodPost, base+"/metrics"); status != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", status)
	}

	// A scraper's keep-alive connection, left open after one scrape.
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /metrics HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	connReader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(connReader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v\n%s", err, g.stderr)
	}
	// Shutting the server down closes open connections too. Closing only the
	// listener would leave this one open.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connReader.ReadByte(); err != io.EOF {
		t.Errorf("keep-alive connection after run returned: read error %v, want EOF", err)
	}
	// The listener is closed once run returns.
	if conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://")); err == nil {
		conn.Close()
		t.Error("metrics address still accepts connections after run returned")
	}
}

func TestRunHTTPServesMetricsApart(t *testing.T) {
	stderr := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, withTestFiles([]string{"-config", fakeConfig(t, "fake"), "-http", "127.0.0.1:0", "-metrics", "127.0.0.1:0"}),
			strings.NewReader(""), io.Discard, stderr, testEnv)
	}()
	mcpURL := waitForHTTP(t, stderr)
	base := waitForMetrics(t, stderr)

	if status, _ := postMCP(t, mcpURL, "tools/call", "fake__echo", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+callParams("fake__echo", `{"text":"hi"}`)+`}`); status != http.StatusOK {
		t.Fatalf("tools/call status = %d", status)
	}
	// MCP and metrics stay on separate addresses.
	if status, _ := scrape(t, http.MethodGet, strings.TrimSuffix(mcpURL, "/mcp")+"/metrics"); status == http.StatusOK {
		t.Error("the MCP address serves /metrics")
	}
	_, text := scrape(t, http.MethodGet, base+"/metrics")
	if !strings.Contains(text, `mcpgw_tool_calls_total{agent="tester",decision="allowed",status="ok",tool="fake__echo"} 1`) {
		t.Errorf("metrics do not count the HTTP call:\n%s", text)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() error = %v\n%s", err, stderr)
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not return in time")
	}
}

func TestRunWithoutMetricsFlagServesNoMetrics(t *testing.T) {
	g := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"))
	g.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + validMeta + `}}`)
	g.next()
	g.in.Close()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if strings.Contains(g.stderr.String(), "metrics listening") {
		t.Error("metrics listener started without -metrics")
	}
}

func TestRunRefusesBusyMetricsPortBeforeStartingAnything(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	cfg := fakeConfig(t, "fake")
	stderr := &syncBuffer{}
	err = run(context.Background(), withTestFiles([]string{"-config", cfg, "-metrics", busy.Addr().String()}),
		strings.NewReader(""), io.Discard, stderr, testEnv)
	if err == nil || !strings.Contains(err.Error(), "listen on "+busy.Addr().String()) {
		t.Fatalf("run() error = %v, want a listen error", err)
	}
	if strings.Contains(stderr.String(), "upstream started") {
		t.Error("an upstream started before the metrics port was checked")
	}
	if _, err := os.Stat(auditDBFor(cfg)); !os.IsNotExist(err) {
		t.Errorf("audit log created before the metrics port was checked (stat error %v)", err)
	}
}
