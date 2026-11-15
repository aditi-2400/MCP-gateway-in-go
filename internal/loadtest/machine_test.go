package loadtest

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestThisMachine(t *testing.T) {
	m := ThisMachine()
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH || m.GoVersion != runtime.Version() || m.CPUs != runtime.NumCPU() {
		t.Errorf("ThisMachine() = %+v", m)
	}
	if m.CPU == "" || m.Commit == "" {
		t.Errorf("empty CPU %q or commit %q; want a value or %q", m.CPU, m.Commit, unknown)
	}
}

func TestLinuxCPUName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpuinfo")
	text := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) CPU @ 2.20GHz\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) CPU @ 2.20GHz\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := linuxCPUName(path); got != "Intel(R) Xeon(R) CPU @ 2.20GHz" {
		t.Errorf("linuxCPUName = %q", got)
	}
	if got := linuxCPUName(filepath.Join(t.TempDir(), "missing")); got != unknown {
		t.Errorf("missing file: %q, want %q", got, unknown)
	}
}

func TestCommitOf(t *testing.T) {
	tests := []struct {
		settings []debug.BuildSetting
		commit   string
		modified bool
	}{
		{nil, unknown, false},
		{[]debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}}, "abc123", false},
		{[]debug.BuildSetting{{Key: "GOARCH", Value: "arm64"}, {Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}, "abc123", true},
	}
	for _, tt := range tests {
		commit, modified := commitOf(tt.settings)
		if commit != tt.commit || modified != tt.modified {
			t.Errorf("commitOf(%v) = %q, %v; want %q, %v", tt.settings, commit, modified, tt.commit, tt.modified)
		}
	}
}
