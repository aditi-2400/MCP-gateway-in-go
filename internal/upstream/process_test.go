package upstream

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeUpstreamBin is the fake MCP server binary, built once in TestMain.
var fakeUpstreamBin string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "upstream-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create temp dir:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	fakeUpstreamBin = filepath.Join(dir, "fakeupstream")
	build := exec.Command("go", "build", "-o", fakeUpstreamBin, "../../testdata/fakeupstream")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fake upstream: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and POSIX signals")
	}
}

func TestProcessCallAndCleanClose(t *testing.T) {
	p, err := Start(Command{Path: fakeUpstreamBin}, Options{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	resp, err := p.Call(testContext(t), "server/discover", []byte(validParams))
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if resp.Error != nil || !strings.Contains(string(resp.Result), `"supportedVersions":["2026-07-28"]`) {
		t.Fatalf("discover response = %s / %v", resp.Result, resp.Error)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v, want clean exit", err)
	}
	if !p.cmd.ProcessState.Success() {
		t.Errorf("process state = %v, want exit status 0", p.cmd.ProcessState)
	}
}

func TestProcessCloseIsIdempotent(t *testing.T) {
	p, err := Start(Command{Path: fakeUpstreamBin}, Options{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	first := p.Close()
	if second := p.Close(); second != first {
		t.Fatalf("second Close() = %v, want same result as first (%v)", second, first)
	}
}

func TestProcessCloseTerminatesUpstreamThatIgnoresStdin(t *testing.T) {
	skipWithoutShell(t)
	// sleep never reads stdin, so closing it does not make it exit. SIGTERM does.
	p, err := Start(Command{Path: "sleep", Args: []string{"30"}}, Options{ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	start := time.Now()
	err = p.Close()
	if err == nil {
		t.Fatal("Close() error = nil, want an error saying the process had to be stopped")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Close() took %v, want it bounded by the shutdown timeout", elapsed)
	}
	if p.cmd.ProcessState == nil {
		t.Error("process was not waited for")
	}
}

func TestProcessCloseKillsUpstreamThatIgnoresSIGTERM(t *testing.T) {
	skipWithoutShell(t)
	// The shell ignores SIGTERM, so only SIGKILL stops it. Its sleep child keeps
	// the stdout pipe open, which Close must also cope with.
	script := `trap "" TERM; sleep 5`
	p, err := Start(Command{Path: "sh", Args: []string{"-c", script}}, Options{ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	start := time.Now()
	if err := p.Close(); err == nil {
		t.Fatal("Close() error = nil, want an error saying the process had to be killed")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Close() took %v, want it bounded by the shutdown timeouts", elapsed)
	}
}

func TestProcessCrashFailsPendingCall(t *testing.T) {
	skipWithoutShell(t)
	// Reads one request, then exits without answering.
	p, err := Start(Command{Path: "sh", Args: []string{"-c", "read line; exit 3"}}, Options{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if _, err := p.Call(testContext(t), "tools/call", []byte(validParams)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call() error = %v, want ErrClosed", err)
	}
	if err := p.Close(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Close() error = %v, want it to report exit status 3", err)
	}
}

func TestStartFailsForMissingCommand(t *testing.T) {
	_, err := Start(Command{Path: filepath.Join(t.TempDir(), "does-not-exist")}, Options{})
	if err == nil {
		t.Fatal("Start() error = nil, want an error")
	}
}
