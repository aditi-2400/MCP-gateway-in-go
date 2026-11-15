package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
)

// mcpServer answers tools/call like the gateway does over HTTP, after
// delay. It records the most calls it saw in flight at once and the
// headers of the first request.
type mcpServer struct {
	delay      time.Duration
	fail       atomic.Bool // answer with an error reply
	refuseEach int64       // refuse every refuseEach-th call like a full audit log
	refuseTill time.Time   // refuse every call before this time
	inFlight   atomic.Int64
	maxSeen    atomic.Int64
	calls      atomic.Int64
	firstMu    sync.Mutex
	firstReq   http.Header
	firstBody  []byte
	connsOpen  atomic.Int64
	connsTotal atomic.Int64
}

func (s *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		m := s.maxSeen.Load()
		if n <= m || s.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	call := s.calls.Add(1)
	body, _ := io.ReadAll(r.Body)
	s.firstMu.Lock()
	if s.firstReq == nil {
		s.firstReq, s.firstBody = r.Header.Clone(), body
	}
	s.firstMu.Unlock()
	time.Sleep(s.delay)

	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")
	if (s.refuseEach > 0 && call%s.refuseEach == 0) || time.Now().Before(s.refuseTill) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32603,"message":"audit log unavailable"}}`))
		return
	}
	if s.fail.Load() {
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32602,"message":"Unknown tool"}}`))
		return
	}
	w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"resultType":"complete","content":[{"type":"text","text":"3"}]}}`))
}

func startMCPServer(t *testing.T, s *mcpServer) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(s)
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			s.connsOpen.Add(1)
			s.connsTotal.Add(1)
		case http.StateClosed, http.StateHijacked:
			s.connsOpen.Add(-1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func TestConcurrentKeepsEveryClientBusy(t *testing.T) {
	s := &mcpServer{delay: 5 * time.Millisecond}
	target := HTTPTarget{URL: startMCPServer(t, s), Key: "mcpgw_k", Tool: "fake__add"}
	got, err := Concurrent(context.Background(), NewHTTPClient(8), target, 8, 50*time.Millisecond, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if s.maxSeen.Load() != 8 {
		t.Errorf("at most %d calls in flight, want 8", s.maxSeen.Load())
	}
	// 8 clients, 5ms a call, 300ms: about 480 calls. Allow for slow machines.
	if got.Calls < 200 || got.Calls > 8*60+8 {
		t.Errorf("measured %d calls, want about 480", got.Calls)
	}
	if got.Clients != 8 || got.Window != 300*time.Millisecond || got.Latency.N != got.Calls {
		t.Errorf("result = %+v", got)
	}
	if want := float64(got.Calls) / 0.3; got.PerSecond != want {
		t.Errorf("PerSecond = %v, want calls / window = %v", got.PerSecond, want)
	}
	if got.Latency.Min < 5*time.Millisecond {
		t.Errorf("min latency %v is below the server's 5ms delay", got.Latency.Min)
	}
	// Warm-up calls happened but weren't counted.
	if total := s.calls.Load(); total <= int64(got.Calls) {
		t.Errorf("server saw %d calls, measured %d: warm-up calls missing", total, got.Calls)
	}
	// Connections were kept and reused, not opened per call.
	if s.connsTotal.Load() > 8 {
		t.Errorf("opened %d connections for 8 clients", s.connsTotal.Load())
	}
}

func TestConcurrentSendsWhatTheGatewayNeeds(t *testing.T) {
	s := &mcpServer{}
	target := HTTPTarget{URL: startMCPServer(t, s), Key: "mcpgw_k", Tool: "fake__add"}
	if _, err := Concurrent(context.Background(), NewHTTPClient(1), target, 1, 0, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Authorization":        "Bearer mcpgw_k",
		"Content-Type":         "application/json",
		"Accept":               "application/json, text/event-stream",
		"Mcp-Protocol-Version": "2026-07-28",
		"Mcp-Method":           "tools/call",
		"Mcp-Name":             "fake__add",
	} {
		if got := s.firstReq.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	var body struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(s.firstBody, &body); err != nil || body.Method != "tools/call" || body.Params.Name != "fake__add" {
		t.Errorf("body %s: %v", s.firstBody, err)
	}
}

func TestConcurrentStopsOnAFailedCall(t *testing.T) {
	s := &mcpServer{}
	s.fail.Store(true)
	target := HTTPTarget{URL: startMCPServer(t, s), Key: "k", Tool: "fake__add"}
	start := time.Now()
	_, err := Concurrent(context.Background(), NewHTTPClient(4), target, 4, 0, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Unknown tool") {
		t.Errorf("error = %v, want the error reply", err)
	}
	if time.Since(start) > time.Second {
		t.Error("kept going after a failed call")
	}
}

func TestConcurrentStopsOnABadStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "audit log unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)
	_, err := Concurrent(context.Background(), NewHTTPClient(2), HTTPTarget{URL: ts.URL, Key: "k", Tool: "t"}, 2, 0, time.Second)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want the status", err)
	}
}

func TestConcurrentStopsWhenCancelled(t *testing.T) {
	s := &mcpServer{delay: time.Millisecond}
	target := HTTPTarget{URL: startMCPServer(t, s), Key: "k", Tool: "t"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Concurrent(ctx, NewHTTPClient(2), target, 2, 0, 5*time.Second)
	if err == nil {
		t.Error("no error after cancel")
	}
	if time.Since(start) > time.Second {
		t.Error("kept going after cancel")
	}
}

func TestConcurrentRejectsBadSettings(t *testing.T) {
	target := HTTPTarget{URL: "http://127.0.0.1:1/mcp", Key: "k", Tool: "t"}
	for _, tc := range []struct {
		clients        int
		warmup, window time.Duration
	}{{0, 0, time.Second}, {1, -time.Second, time.Second}, {1, 0, 0}} {
		if _, err := Concurrent(context.Background(), NewHTTPClient(1), target, tc.clients, tc.warmup, tc.window); err == nil {
			t.Errorf("%+v: no error", tc)
		}
	}
}

func TestStartHTTPGatewayServesCalls(t *testing.T) {
	key, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	args, err := WriteGatewayFiles(t.TempDir(), fakeUpstreamBin, auth.HashKey(key))
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	gw, err := StartHTTPGateway(gatewayBin, args, &stderr)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	target := HTTPTarget{URL: gw.URL, Key: key, Tool: UpstreamName + "__add"}
	got, err := Concurrent(context.Background(), NewHTTPClient(4), target, 4, 20*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if got.Calls == 0 {
		t.Error("no calls measured")
	}
	if err := gw.Stop(); err != nil {
		t.Errorf("stop: %v\n%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), key) {
		t.Error("gateway logged the key")
	}
}

func TestStartHTTPGatewayReportsAGatewayThatExits(t *testing.T) {
	var stderr bytes.Buffer
	_, err := StartHTTPGateway(gatewayBin, []string{"-config", "/no/such/file"}, &stderr)
	if err == nil {
		t.Fatal("no error for a gateway that can't start")
	}
}

// Refusals from a full audit log are counted, not treated as failures, and
// never mixed into the rate or the latency.
func TestConcurrentCountsRefusalsApart(t *testing.T) {
	s := &mcpServer{delay: time.Millisecond, refuseEach: 4}
	target := HTTPTarget{URL: startMCPServer(t, s), Key: "k", Tool: "fake__add"}
	got, err := Concurrent(context.Background(), NewHTTPClient(4), target, 4, 20*time.Millisecond, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refused == 0 || got.Calls == 0 {
		t.Fatalf("calls %d, refused %d; want both above zero", got.Calls, got.Refused)
	}
	// One call in four is refused, so about three answered per refusal.
	if ratio := float64(got.Calls) / float64(got.Refused); ratio < 2 || ratio > 4 {
		t.Errorf("calls %d, refused %d: want about 3 answered per refused", got.Calls, got.Refused)
	}
	if got.Latency.N != got.Calls {
		t.Errorf("latency has %d samples, want only the %d answered calls", got.Latency.N, got.Calls)
	}
	if want := float64(got.Calls) / 0.3; got.PerSecond != want {
		t.Errorf("PerSecond = %v, want answered calls / window = %v", got.PerSecond, want)
	}
}

// Only the audit log's refusal is counted; any other internal error stops.
func TestConcurrentStopsOnOtherInternalErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"upstream exited"}}`))
	}))
	t.Cleanup(ts.Close)
	_, err := Concurrent(context.Background(), NewHTTPClient(1), HTTPTarget{URL: ts.URL, Key: "k", Tool: "t"}, 1, 0, time.Second)
	if err == nil || !strings.Contains(err.Error(), "upstream exited") {
		t.Errorf("error = %v, want the internal error", err)
	}
}

// Refusals during warm-up aren't counted, just like warm-up calls.
func TestConcurrentIgnoresRefusalsDuringWarmUp(t *testing.T) {
	s := &mcpServer{delay: time.Millisecond, refuseTill: time.Now().Add(100 * time.Millisecond)}
	target := HTTPTarget{URL: startMCPServer(t, s), Key: "k", Tool: "fake__add"}
	got, err := Concurrent(context.Background(), NewHTTPClient(2), target, 2, 200*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refused != 0 || got.Calls == 0 {
		t.Errorf("calls %d, refused %d; want refusals only in warm-up, so none counted", got.Calls, got.Refused)
	}
}
