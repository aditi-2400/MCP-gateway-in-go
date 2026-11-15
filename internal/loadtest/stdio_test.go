package loadtest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/auth"
	"github.com/aditi-2400/MCP-gateway-in-go/internal/upstream"
)

// Built once in TestMain.
var fakeUpstreamBin, gatewayBin string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "loadtest-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create temp dir:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	fakeUpstreamBin = filepath.Join(dir, "fakeupstream")
	gatewayBin = filepath.Join(dir, "gateway")
	for out, pkg := range map[string]string{
		fakeUpstreamBin: "../../testdata/fakeupstream",
		gatewayBin:      "../../cmd/gateway",
	} {
		if output, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, output)
			return 1
		}
	}
	return m.Run()
}

func TestMeasureStdioDirect(t *testing.T) {
	target := StdioTarget{Name: "direct", Command: upstream.Command{Path: fakeUpstreamBin}, Tool: "add"}
	times, err := MeasureStdio(context.Background(), target, 5, 20, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 20 {
		t.Errorf("got %d times, want 20", len(times))
	}
}

// gatewayTarget sets up a gateway in front of the fake upstream, with key
// as the stdio key and the config's hash made from configKey.
func gatewayTarget(t *testing.T, key, configKey string) StdioTarget {
	t.Helper()
	args, err := WriteGatewayFiles(t.TempDir(), fakeUpstreamBin, auth.HashKey(configKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(KeyEnv, key)
	return StdioTarget{Name: "gateway", Command: upstream.Command{Path: gatewayBin, Args: args}, Tool: UpstreamName + "__add"}
}

func TestMeasureStdioThroughTheGateway(t *testing.T) {
	key, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	times, err := MeasureStdio(context.Background(), gatewayTarget(t, key, key), 5, 20, &stderr)
	if err != nil {
		t.Fatalf("%v\ngateway stderr:\n%s", err, stderr.String())
	}
	if len(times) != 20 {
		t.Errorf("got %d times, want 20", len(times))
	}
}

// A gateway that refuses to start is an error, never an empty result.
func TestMeasureStdioReportsAGatewayThatExits(t *testing.T) {
	var stderr bytes.Buffer
	_, err := MeasureStdio(context.Background(), gatewayTarget(t, "mcpgw_wrong", "mcpgw_right"), 0, 5, &stderr)
	if err == nil {
		t.Fatal("no error with a wrong key")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("error = %q, want it to name the target", err)
	}
	if strings.Contains(stderr.String(), "mcpgw_wrong") {
		t.Error("gateway logged the key")
	}
}

// A target that answers every call but then exits with an error still fails
// the run: something went wrong that the numbers can't show.
func TestMeasureStdioReportsAFailedExit(t *testing.T) {
	skipWithoutShell(t)
	target := StdioTarget{
		Name:    "direct",
		Command: upstream.Command{Path: "sh", Args: []string{"-c", `"$0"; exit 3`, fakeUpstreamBin}},
		Tool:    "add",
	}
	_, err := MeasureStdio(context.Background(), target, 0, 5, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Errorf("error = %v, want a stop error", err)
	}
}

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
}
