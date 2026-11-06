package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []Upstream
	}{
		{
			name: "one upstream without args",
			yaml: "upstreams:\n  - name: fs\n    command: ./server\n",
			want: []Upstream{{Name: "fs", Command: "./server"}},
		},
		{
			name: "two upstreams with args and comments",
			yaml: `
# Local tools.
upstreams:
  - name: fs
    command: /usr/local/bin/fs-server
    args: ["--root", "/tmp/agent-workspace"]
  - name: github-2   # digits and dashes are fine
    command: gh-server
    args:
      - --verbose
`,
			want: []Upstream{
				{Name: "fs", Command: "/usr/local/bin/fs-server", Args: []string{"--root", "/tmp/agent-workspace"}},
				{Name: "github-2", Command: "gh-server", Args: []string{"--verbose"}},
			},
		},
		{
			name: "number arg is read as a string",
			yaml: "upstreams:\n  - name: db\n    command: db-server\n    args: [--port, 5432]\n",
			want: []Upstream{{Name: "db", Command: "db-server", Args: []string{"--port", "5432"}}},
		},
		{
			name: "longest allowed name",
			yaml: "upstreams:\n  - name: " + strings.Repeat("a", maxNameLength) + "\n    command: x\n",
			want: []Upstream{{Name: strings.Repeat("a", maxNameLength), Command: "x"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("parse() error = %v", err)
			}
			if !reflect.DeepEqual(cfg.Upstreams, tt.want) {
				t.Fatalf("upstreams = %#v\nwant %#v", cfg.Upstreams, tt.want)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		// Every string must appear in the error.
		wantErr []string
	}{
		{name: "empty file", yaml: "", wantErr: []string{"parse"}},
		{name: "only a comment", yaml: "# nothing here\n", wantErr: []string{"parse"}},
		{name: "not yaml", yaml: "upstreams: [\n", wantErr: []string{"parse"}},
		{name: "two documents", yaml: "upstreams: []\n---\nupstreams: []\n", wantErr: []string{"parse"}},
		{name: "no upstreams key", yaml: "other: 1\n", wantErr: []string{"parse"}},
		{name: "empty upstream list", yaml: "upstreams: []\n", wantErr: []string{"no upstreams"}},
		{name: "upstreams is a map", yaml: "upstreams: {fs: x}\n", wantErr: []string{"parse"}},
		{name: "typo in field", yaml: "upstreams:\n  - name: fs\n    comand: ./server\n", wantErr: []string{"parse", "comand"}},
		{name: "unknown top-level field", yaml: "upstream:\n  - name: fs\n", wantErr: []string{"parse", "upstream"}},
		{name: "duplicate key", yaml: "upstreams:\n  - name: fs\n    name: db\n    command: x\n", wantErr: []string{"parse"}},
		{name: "args is a string", yaml: "upstreams:\n  - name: fs\n    command: x\n    args: --root /tmp\n", wantErr: []string{"parse"}},
		{name: "missing name", yaml: "upstreams:\n  - command: x\n", wantErr: []string{"upstreams[0]", "name is required"}},
		{name: "missing command", yaml: "upstreams:\n  - name: fs\n", wantErr: []string{"upstreams[0]", "command is required"}},
		{name: "blank command", yaml: "upstreams:\n  - name: fs\n    command: \"  \"\n", wantErr: []string{"command is required"}},
		{name: "uppercase name", yaml: "upstreams:\n  - name: FS\n    command: x\n", wantErr: []string{`"FS"`}},
		{name: "underscore in name", yaml: "upstreams:\n  - name: my_fs\n    command: x\n", wantErr: []string{`"my_fs"`}},
		{name: "double underscore in name", yaml: "upstreams:\n  - name: a__b\n    command: x\n", wantErr: []string{`"a__b"`}},
		{name: "name starts with digit", yaml: "upstreams:\n  - name: 1fs\n    command: x\n", wantErr: []string{`"1fs"`}},
		{name: "name starts with dash", yaml: "upstreams:\n  - name: -fs\n    command: x\n", wantErr: []string{`"-fs"`}},
		{name: "space in name", yaml: "upstreams:\n  - name: \"f s\"\n    command: x\n", wantErr: []string{`"f s"`}},
		{name: "name too long", yaml: "upstreams:\n  - name: " + strings.Repeat("a", maxNameLength+1) + "\n    command: x\n", wantErr: []string{"at most 32"}},
		{
			name:    "duplicate name",
			yaml:    "upstreams:\n  - name: fs\n    command: x\n  - name: fs\n    command: y\n",
			wantErr: []string{"upstreams[1]", `"fs"`, "upstreams[0]"},
		},
		{
			name:    "all problems reported together",
			yaml:    "upstreams:\n  - name: Bad\n  - command: y\n",
			wantErr: []string{"upstreams[0]", `"Bad"`, "command is required", "upstreams[1]", "name is required"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("parse() error = nil, want an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "testdata", "config", "example.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Upstreams) == 0 {
		t.Fatal("example config has no upstreams")
	}
}

func TestLoadErrorsNameTheFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("upstreams: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "missing file", path: filepath.Join(dir, "missing.yaml")},
		{name: "invalid file", path: bad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path)
			if err == nil || !strings.Contains(err.Error(), tt.path) {
				t.Fatalf("Load() error = %v, want it to name %s", err, tt.path)
			}
		})
	}
}
