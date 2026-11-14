package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const examplePolicy = `
roles:
  readers:
    allow:
      - tool: fs__read_file
      - tool: github__*
  db-readers:
    allow:
      - tool: db__query
agents:
  ci-bot: [readers]
  analyst: [readers, db-readers]
  idle: []
`

func mustParse(t *testing.T, text string) *Policy {
	t.Helper()
	p, err := parse([]byte(text))
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	return p
}

func TestAllowTool(t *testing.T) {
	p := mustParse(t, examplePolicy)
	tests := []struct {
		name  string
		agent string
		tool  string
		allow bool
		rule  string // the matching rule, when allowed
	}{
		{name: "exact rule", agent: "ci-bot", tool: "fs__read_file", allow: true, rule: "readers: fs__read_file"},
		{name: "upstream wildcard", agent: "ci-bot", tool: "github__create_issue", allow: true, rule: "readers: github__*"},
		{name: "second role", agent: "analyst", tool: "db__query", allow: true, rule: "db-readers: db__query"},
		{name: "first role still applies", agent: "analyst", tool: "fs__read_file", allow: true},

		{name: "tool not in any role", agent: "ci-bot", tool: "fs__write_file"},
		{name: "other role's tool", agent: "ci-bot", tool: "db__query"},
		{name: "agent with no roles", agent: "idle", tool: "fs__read_file"},
		{name: "agent not in policy", agent: "stranger", tool: "fs__read_file"},
		{name: "empty agent", agent: "", tool: "fs__read_file"},
		{name: "empty tool", agent: "ci-bot", tool: ""},
		{name: "exact rule is not a prefix", agent: "ci-bot", tool: "fs__read_file_and_delete"},
		{name: "case differs", agent: "ci-bot", tool: "FS__read_file"},
		{name: "wildcard needs a tool part", agent: "ci-bot", tool: "github__"},
		{name: "wildcard stays in its upstream", agent: "ci-bot", tool: "github-enterprise__create_issue"},
		{name: "wildcard needs the separator", agent: "ci-bot", tool: "githubx"},
		{name: "no separator", agent: "ci-bot", tool: "read_file"},
		{name: "space trick", agent: "ci-bot", tool: "fs__read_file "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.AllowTool(tt.agent, tt.tool)
			if d.Allowed != tt.allow {
				t.Fatalf("AllowTool(%q, %q) = %+v, want allowed=%v", tt.agent, tt.tool, d, tt.allow)
			}
			if d.Reason == "" {
				t.Error("decision has no reason")
			}
			if tt.rule != "" && d.Rule != tt.rule {
				t.Errorf("rule = %q, want %q", d.Rule, tt.rule)
			}
			if !d.Allowed && d.Rule != "" {
				t.Errorf("denied decision names rule %q", d.Rule)
			}
		})
	}
}

func TestParseRejectsBadPolicies(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr []string
	}{
		{name: "empty file", text: "", wantErr: []string{"parse"}},
		{name: "unknown field", text: "roles: {}\nagents: {}\nallow_all: true\n", wantErr: []string{"parse", "allow_all"}},
		{name: "typo in rule", text: "roles:\n  r:\n    allow:\n      - tol: fs__x\nagents:\n  a: [r]\n", wantErr: []string{"parse", "tol"}},
		{name: "no agents", text: "roles:\n  r:\n    allow:\n      - tool: fs__x\n", wantErr: []string{"no agents"}},
		{name: "unknown role", text: "roles:\n  r:\n    allow:\n      - tool: fs__x\nagents:\n  a: [r, admin]\n", wantErr: []string{`agent "a"`, `"admin"`}},
		{name: "role listed twice", text: "roles:\n  r:\n    allow:\n      - tool: fs__x\nagents:\n  a: [r, r]\n", wantErr: []string{`agent "a"`, "twice"}},
		{name: "bad role name", text: "roles:\n  Readers:\n    allow:\n      - tool: fs__x\nagents:\n  a: [Readers]\n", wantErr: []string{`"Readers"`}},
		{name: "bad agent name", text: "roles:\n  r:\n    allow:\n      - tool: fs__x\nagents:\n  CI_Bot: [r]\n", wantErr: []string{`"CI_Bot"`}},
		{name: "role with no rules", text: "roles:\n  r:\n    allow: []\nagents:\n  a: [r]\n", wantErr: []string{`role "r"`, "no rules"}},
		{name: "missing tool", text: "roles:\n  r:\n    allow:\n      - {}\nagents:\n  a: [r]\n", wantErr: []string{`role "r"`, "allow[0]", "tool is required"}},
		{name: "no separator", text: "roles:\n  r:\n    allow:\n      - tool: read_file\nagents:\n  a: [r]\n", wantErr: []string{"allow[0]", `"read_file"`}},
		{name: "bare star", text: "roles:\n  r:\n    allow:\n      - tool: \"*\"\nagents:\n  a: [r]\n", wantErr: []string{`"*"`}},
		{name: "star across upstreams", text: "roles:\n  r:\n    allow:\n      - tool: \"*__read_file\"\nagents:\n  a: [r]\n", wantErr: []string{`"*__read_file"`}},
		{name: "partial star", text: "roles:\n  r:\n    allow:\n      - tool: \"fs__read_*\"\nagents:\n  a: [r]\n", wantErr: []string{`"fs__read_*"`}},
		{name: "empty tool part", text: "roles:\n  r:\n    allow:\n      - tool: fs__\nagents:\n  a: [r]\n", wantErr: []string{`"fs__"`}},
		{name: "bad upstream part", text: "roles:\n  r:\n    allow:\n      - tool: FS__x\nagents:\n  a: [r]\n", wantErr: []string{`"FS__x"`}},
		{name: "space in tool", text: "roles:\n  r:\n    allow:\n      - tool: \"fs__read file\"\nagents:\n  a: [r]\n", wantErr: []string{`"fs__read file"`}},
		{name: "duplicate role key", text: "roles:\n  r:\n    allow:\n      - tool: fs__x\n  r:\n    allow:\n      - tool: fs__y\nagents:\n  a: [r]\n", wantErr: []string{"parse"}},
		{
			name:    "all problems together",
			text:    "roles:\n  r:\n    allow:\n      - tool: nope\n      - tool: fs__\nagents:\n  a: [missing]\n",
			wantErr: []string{"allow[0]", "allow[1]", `"missing"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.text))
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

func TestCheckNames(t *testing.T) {
	p := mustParse(t, examplePolicy)
	if err := p.CheckNames([]string{"fs", "github", "db"}, []string{"ci-bot", "analyst", "idle", "extra"}); err != nil {
		t.Fatalf("CheckNames() = %v, want nil (config agents without a policy entry are fine: they get nothing)", err)
	}

	err := p.CheckNames([]string{"fs", "db"}, []string{"ci-bot", "idle"})
	if err == nil {
		t.Fatal("CheckNames() = nil, want errors")
	}
	for _, want := range []string{`"github__*"`, `upstream "github"`, `"analyst"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(good, []byte(examplePolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("roles: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bad, filepath.Join(dir, "missing.yaml")} {
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("Load(%s) error = %v, want one naming the file", path, err)
		}
	}
}

func TestLoadExamplePolicy(t *testing.T) {
	p, err := Load(filepath.Join("..", "..", "testdata", "config", "example-policy.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if d := p.AllowCall("example", "fake__echo", json.RawMessage(`{"text":"hi"}`)); !d.Allowed {
		t.Errorf("example policy denies fake__echo: %s", d.Reason)
	}
	if d := p.AllowCall("example", "fake__echo", json.RawMessage(`{"text":"my Password is x"}`)); d.Allowed {
		t.Error("example policy allows fake__echo text matching its deny pattern")
	}
}
