package upstream

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// answerDiscoverThenExit is a stand-in server that answers the startup
// server/discover (always request id 1 on a new Conn) with these supported
// versions, then exits.
func answerDiscoverThenExit(versions string) Command {
	reply := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":` + versions + `,"capabilities":{"tools":{}}}}`
	return Command{Path: "sh", Args: []string{"-c", `read line; printf '%s\n' '` + reply + `'; sleep 0.2`}}
}

// fastOptions keeps failing startups and shutdowns quick in tests.
func fastOptions() Options {
	return Options{Stderr: io.Discard, StartupTimeout: 300 * time.Millisecond, ShutdownTimeout: 300 * time.Millisecond}
}

func TestStartGroupStartsEveryUpstream(t *testing.T) {
	g, err := StartGroup(testContext(t), []Spec{
		{Name: "a", Command: Command{Path: fakeUpstreamBin}},
		{Name: "b", Command: Command{Path: fakeUpstreamBin}},
	}, Options{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("StartGroup() error = %v", err)
	}

	members := g.Members()
	if len(members) != 2 || members[0].Name != "a" || members[1].Name != "b" {
		t.Fatalf("members = %+v, want a then b", members)
	}
	for _, m := range members {
		resp, err := m.Process.Call(testContext(t), "tools/list", []byte(validParams))
		if err != nil || resp.Error != nil {
			t.Fatalf("%s: tools/list = %v, %v", m.Name, resp, err)
		}
	}

	if err := g.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	for _, m := range members {
		select {
		case <-m.Process.Done():
		default:
			t.Errorf("%s still running after Close", m.Name)
		}
	}
	if err := g.Close(); err != nil {
		t.Errorf("second Close() error = %v, want the same nil", err)
	}
}

func TestStartGroupFailures(t *testing.T) {
	skipWithoutShell(t)
	missing := filepath.Join(t.TempDir(), "no-such-server")

	tests := []struct {
		name    string
		command Command
		wantErr string
	}{
		{name: "program not found", command: Command{Path: missing}, wantErr: "start"},
		{name: "exits right away", command: Command{Path: "sh", Args: []string{"-c", "exit 3"}}, wantErr: "server/discover"},
		{name: "never answers", command: Command{Path: "sleep", Args: []string{"30"}}, wantErr: "server/discover"},
		{name: "answers with an error", command: Command{Path: "sh", Args: []string{"-c",
			`read line; printf '%s\n' '{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}'; sleep 0.2`}}, wantErr: "Method not found"},
		{name: "wrong protocol version", command: answerDiscoverThenExit(`["2025-11-25"]`), wantErr: "2026-07-28"},
		{name: "result is not an object", command: Command{Path: "sh", Args: []string{"-c",
			`read line; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":[1]}'; sleep 0.2`}}, wantErr: "server/discover"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := StartGroup(testContext(t), []Spec{{Name: "bad", Command: tt.command}}, fastOptions())
			if err == nil {
				g.Close()
				t.Fatal("StartGroup() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), `"bad"`) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to name \"bad\" and mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestStartGroupFailureStopsUpstreamsAlreadyStarted(t *testing.T) {
	skipWithoutShell(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	// exec keeps the same PID, so the file holds the fake server's PID.
	first := Command{Path: "sh", Args: []string{"-c", `echo $$ > "$0"; exec "$1"`, pidFile, fakeUpstreamBin}}

	_, err := StartGroup(testContext(t), []Spec{
		{Name: "good", Command: first},
		{Name: "bad", Command: Command{Path: filepath.Join(t.TempDir(), "missing")}},
	}, fastOptions())
	if err == nil || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("StartGroup() error = %v, want an error naming \"bad\"", err)
	}

	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read pid file: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil {
		t.Fatalf("parse pid %q: %v", data, convErr)
	}
	// Signal 0 only checks whether the process exists.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("first upstream (pid %d) still exists: kill(0) = %v", pid, err)
	}
}

func TestGroupDoneFiresWhenAnyMemberExits(t *testing.T) {
	skipWithoutShell(t)
	g, err := StartGroup(testContext(t), []Spec{
		{Name: "steady", Command: Command{Path: fakeUpstreamBin}},
		{Name: "flaky", Command: answerDiscoverThenExit(`["2026-07-28"]`)},
	}, fastOptions())
	if err != nil {
		t.Fatalf("StartGroup() error = %v", err)
	}
	defer g.Close()

	select {
	case <-g.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire after a member exited")
	}
	if err := g.Err(); err == nil || !strings.Contains(err.Error(), `"flaky"`) || !errors.Is(err, ErrClosed) {
		t.Fatalf("Err() = %v, want ErrClosed naming \"flaky\"", err)
	}
}

func TestStartGroupNeedsUpstreams(t *testing.T) {
	if _, err := StartGroup(testContext(t), nil, Options{}); err == nil {
		t.Fatal("StartGroup(nil) error = nil, want an error")
	}
}
