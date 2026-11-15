package loadtest

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
)

const unknown = "unknown"

// ThisMachine describes the computer and the build doing the measuring.
// Anything it can't find out is "unknown"; that never stops a run.
func ThisMachine() Machine {
	m := Machine{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		CPU:       cpuName(),
		CPUs:      runtime.NumCPU(),
		GoVersion: runtime.Version(),
		Commit:    unknown,
	}
	// go build stamps the git commit into the binary; go run and go test don't.
	if info, ok := debug.ReadBuildInfo(); ok {
		m.Commit, m.Modified = commitOf(info.Settings)
	}
	return m
}

// commitOf reads the git commit and whether there were uncommitted changes
// from a binary's build settings.
func commitOf(settings []debug.BuildSetting) (commit string, modified bool) {
	commit = unknown
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return commit, modified
}

// cpuName returns the processor's model name, e.g. "Apple M3".
func cpuName() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		if err != nil {
			return unknown
		}
		return strings.TrimSpace(string(out))
	case "linux":
		return linuxCPUName("/proc/cpuinfo")
	default:
		return unknown
	}
}

// linuxCPUName reads the first "model name" line of a /proc/cpuinfo file.
func linuxCPUName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return unknown
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return unknown
}
