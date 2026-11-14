package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

var listenLine = regexp.MustCompile(`msg="http listening" addr=(\S+)`)

// waitForHTTP waits for the gateway to log its listen address and returns
// the endpoint URL.
func waitForHTTP(t *testing.T, stderr *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if m := listenLine.FindStringSubmatch(stderr.String()); m != nil {
			return "http://" + m[1] + "/mcp"
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gateway did not log a listen address\nstderr:\n%s", stderr)
	return ""
}

// postMCP sends one MCP request with the headers the spec requires.
func postMCP(t *testing.T, url, method, name, body string) (int, *jsonrpc.Message) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := jsonrpc.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", data, err)
	}
	return resp.StatusCode, msg
}

func TestRunServesHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := startGateway(t, ctx, "-config", fakeConfig(t, "alpha", "beta"), "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	status, resp := postMCP(t, url, "tools/list", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+validMeta+`}}`)
	if status != http.StatusOK || resp.Error != nil || !strings.Contains(string(resp.Result), `"name":"beta__echo"`) {
		t.Fatalf("tools/list = %d %s %v", status, resp.Result, resp.Error)
	}
	status, resp = postMCP(t, url, "tools/call", "alpha__add", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"alpha__add","arguments":{"a":2,"b":3},`+validMeta+`}}`)
	if status != http.StatusOK || !strings.Contains(string(resp.Result), `"sum":5`) {
		t.Fatalf("tools/call = %d %s %v", status, resp.Result, resp.Error)
	}
	status, _ = postMCP(t, url, "resources/list", "", `{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{`+validMeta+`}}`)
	if status != http.StatusNotFound {
		t.Fatalf("resources/list status = %d, want 404", status)
	}

	// In HTTP mode stdin is not read, so closing it changes nothing.
	g.in.Close()
	status, _ = postMCP(t, url, "tools/list", "", `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{`+validMeta+`}}`)
	if status != http.StatusOK {
		t.Fatalf("after stdin closed, status = %d, want 200", status)
	}

	cancel()
	if err := g.wait(); err != nil {
		t.Fatalf("run() error = %v, want nil on signal\nstderr:\n%s", err, g.stderr)
	}
	if _, err := http.Post(url, "application/json", strings.NewReader("{}")); err == nil {
		t.Error("server still accepts connections after shutdown")
	}
}

func TestRunHTTPFailsWhenAnUpstreamExits(t *testing.T) {
	skipWithoutShell(t)
	cfg := writeConfig(t, upstreamEntry{name: "steady", command: fakeUpstreamBin}, discoverThenExit)
	g := startGateway(t, context.Background(), "-config", cfg, "-http", "127.0.0.1:0")

	err := g.wait()
	if !errors.Is(err, errUpstreamExited) || !strings.Contains(err.Error(), `"flaky"`) {
		t.Fatalf("run() error = %v, want errUpstreamExited naming \"flaky\"", err)
	}
}

func TestRunHTTPReportsAddressInUse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := startGateway(t, ctx, "-config", fakeConfig(t, "fake"), "-http", "127.0.0.1:0")
	addr := strings.TrimSuffix(strings.TrimPrefix(waitForHTTP(t, first.stderr), "http://"), "/mcp")

	second := startGateway(t, context.Background(), "-config", fakeConfig(t, "fake"), "-http", addr)
	if err := second.wait(); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("second gateway error = %v, want a listen error", err)
	}
}

func TestBinaryServesHTTPAndStopsOnSIGTERM(t *testing.T) {
	skipWithoutShell(t)
	g := startGatewayProcess(t, "-config", fakeConfig(t, "alpha", "beta"), "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	status, resp := postMCP(t, url, "tools/call", "beta__echo", `{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"beta__echo","arguments":{"text":"over http"},`+validMeta+`}}`)
	if status != http.StatusOK || resp.ID == nil || *resp.ID != jsonrpc.NewStringID("x") || !strings.Contains(string(resp.Result), `"text":"over http"`) {
		t.Fatalf("tools/call = %d id %v %s %v", status, resp.ID, resp.Result, resp.Error)
	}

	if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	if code := g.exitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, g.stderr)
	}
}

func TestBinaryRefusesNonLoopbackHTTP(t *testing.T) {
	g := startGatewayProcess(t, "-config", fakeConfig(t, "fake"), "-http", "0.0.0.0:0")
	if code := g.exitCode(); code != 2 {
		t.Fatalf("exit code = %d, want 2\nstderr:\n%s", code, g.stderr)
	}
	if !strings.Contains(g.stderr.String(), "loopback") {
		t.Errorf("stderr = %q, want the loopback rule explained", g.stderr)
	}
}

// postSSE sends a request that asks for progress and returns every event.
func postSSE(t *testing.T, url, name, id, token string, n int) []*jsonrpc.Message {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + name + `","arguments":{"n":` + strconv.Itoa(n) + `},` +
		`"_meta":{"progressToken":` + token + `,"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Error(err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", name)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Error(err)
		return nil
	}
	var msgs []*jsonrpc.Message
	for _, block := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
		_, payload, _ := strings.Cut(block, "data: ")
		msg, err := jsonrpc.Parse([]byte(payload))
		if err != nil {
			t.Errorf("parse event %q: %v", block, err)
			return nil
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestRunHTTPStreamsProgressToTheRightClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := startGateway(t, ctx, "-config", fakeConfig(t, "alpha", "beta"), "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	// Both clients pick progress token "1". Each must get only its own steps.
	type result struct {
		n    int
		msgs []*jsonrpc.Message
	}
	results := make(chan result, 2)
	for _, n := range []int{3, 5} {
		go func() {
			results <- result{n, postSSE(t, url, "beta__count", strconv.Itoa(n), `"1"`, n)}
		}()
	}
	for range 2 {
		r := <-results
		if len(r.msgs) != r.n+1 {
			t.Fatalf("client n=%d got %d events, want %d progress and 1 result", r.n, len(r.msgs), r.n)
		}
		for i, msg := range r.msgs[:r.n] {
			want := `{"progress":` + strconv.Itoa(i+1) + `,"progressToken":"1","total":` + strconv.Itoa(r.n) + `}`
			if msg.Method != "notifications/progress" || string(msg.Params) != want {
				t.Errorf("client n=%d event %d = %s, want %s", r.n, i, msg.Params, want)
			}
		}
		last := r.msgs[r.n]
		if last.ID == nil || *last.ID != jsonrpc.NewIntID(int64(r.n)) || !strings.Contains(string(last.Result), "counted to "+strconv.Itoa(r.n)) {
			t.Errorf("client n=%d last event = %+v, want its own result", r.n, last)
		}
	}
}

// postLookup calls the fake lookup tool, whose schema mirrors region into
// Mcp-Param-Region, with the given extra headers.
func postLookup(t *testing.T, url string, headers map[string]string) (int, *jsonrpc.Message) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"alpha__lookup","arguments":{"region":"us-west1","limit":5,"query":"q"},` + validMeta + `}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "alpha__lookup")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	msg, err := jsonrpc.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", data, err)
	}
	return resp.StatusCode, msg
}

func TestRunHTTPChecksMcpParamHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := startGateway(t, ctx, "-config", fakeConfig(t, "alpha"), "-http", "127.0.0.1:0")
	url := waitForHTTP(t, g.stderr)

	// No tools/list first: the gateway has to fetch the schema on its own.
	status, resp := postLookup(t, url, map[string]string{"Mcp-Param-Region": "us-west1", "Mcp-Param-Limit": "5"})
	if status != http.StatusOK || resp.Error != nil || !strings.Contains(string(resp.Result), `"region":"us-west1"`) {
		t.Fatalf("matching headers: %d %s %v", status, resp.Result, resp.Error)
	}

	for name, headers := range map[string]map[string]string{
		"missing Region":   {"Mcp-Param-Limit": "5"},
		"wrong Region":     {"Mcp-Param-Region": "eu-west1", "Mcp-Param-Limit": "5"},
		"missing Limit":    {"Mcp-Param-Region": "us-west1"},
		"Verbose not sent": {"Mcp-Param-Region": "us-west1", "Mcp-Param-Limit": "5", "Mcp-Param-Verbose": "true"},
	} {
		status, resp := postLookup(t, url, headers)
		if status != http.StatusBadRequest || resp.Error == nil || resp.Error.Code != -32020 {
			t.Errorf("%s: %d %v, want 400 with -32020", name, status, resp.Error)
		}
	}
}

// writeSelfSignedCert makes a throwaway certificate for 127.0.0.1 and
// returns the cert and key file paths, plus a pool that trusts it.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gateway test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func TestRunServesHTTPS(t *testing.T) {
	certFile, keyFile, pool := writeSelfSignedCert(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := startGateway(t, ctx, "-config", fakeConfig(t, "fake"), "-http", "127.0.0.1:0", "-tls-cert", certFile, "-tls-key", keyFile)
	if !strings.Contains(waitForLog(t, g.stderr, "scheme=https"), "scheme=https") {
		t.Fatal("listen log does not say https")
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(waitForHTTP(t, g.stderr), "http://"), "/mcp")

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+validMeta+`}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"fake__echo"`) {
		t.Fatalf("HTTPS tools/list = %d %s", resp.StatusCode, body)
	}

	// Plain HTTP to the TLS port must not get through.
	plain, err := http.Post("http://"+addr+"/mcp", "application/json", strings.NewReader("{}"))
	if err == nil {
		plain.Body.Close()
		if plain.StatusCode != http.StatusBadRequest {
			t.Errorf("plain HTTP to the TLS port: status %d, want 400", plain.StatusCode)
		}
	}
}

func TestRunRejectsBadTLSFilesBeforeStartingAnything(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(bad, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	// syncBuffer, because upstream child processes also write to stderr.
	stderr := &syncBuffer{}
	err := run(context.Background(), []string{"-config", fakeConfig(t, "fake"), "-http", "127.0.0.1:0", "-tls-cert", bad, "-tls-key", bad},
		strings.NewReader(""), io.Discard, stderr, testEnv)
	if err == nil || !strings.Contains(err.Error(), "TLS certificate") {
		t.Fatalf("run() error = %v, want a TLS certificate error", err)
	}
	for _, line := range []string{"upstream started", "http listening"} {
		if strings.Contains(stderr.String(), line) {
			t.Errorf("logged %q before the certificate was checked", line)
		}
	}
}

// waitForLog waits until stderr contains want and returns everything so far.
func waitForLog(t *testing.T, stderr *syncBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if s := stderr.String(); strings.Contains(s, want) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stderr never contained %q:\n%s", want, stderr)
	return ""
}
