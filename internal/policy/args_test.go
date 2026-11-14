package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

const argsPolicy = `
roles:
  writer:
    allow:
      - tool: fs__write_file
        args:
          path: { path_prefix: [/tmp/agent-workspace, /srv/shared] }
          content: { max_length: 20 }
          mode: {}
      - tool: fs__read_file
      - tool: db__query
        args:
          sql: { deny_patterns: ['\bdrop\b', '\btruncate\b'] }
  other-writer:
    allow:
      - tool: fs__write_file
        args:
          path: { path_prefix: [/var/other] }
  no-args:
    allow:
      - tool: fs__ping
        args: {}
agents:
  bot: [writer]
  both: [writer, other-writer]
  pinger: [no-args]
`

func TestAllowCall(t *testing.T) {
	p := mustParse(t, argsPolicy)
	write := func(path string) string {
		return `{"path":` + path + `,"content":"hi"}`
	}
	tests := []struct {
		name   string
		agent  string
		tool   string
		args   string // raw arguments JSON; empty means none sent
		allow  bool
		reason string // part of the reason, when denied
	}{
		// Paths that stay inside.
		{name: "inside the folder", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/notes.txt"`), allow: true},
		{name: "the folder itself", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace"`), allow: true},
		{name: "second prefix", agent: "bot", tool: "fs__write_file", args: write(`"/srv/shared/x"`), allow: true},
		{name: "dot-dot that stays inside", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/a/../b.txt"`), allow: true},
		{name: "double slashes", agent: "bot", tool: "fs__write_file", args: write(`"//tmp//agent-workspace//x"`), allow: true},
		// The filesystem server does not decode %2e, so to it this is a
		// folder literally named "%2e%2e" inside the workspace.
		{name: "percent-encoding is a literal name", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/%2e%2e/%2e%2e/etc/passwd"`), allow: true},

		// Traversal and lookalikes.
		{name: "traversal", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/../../etc/passwd"`), reason: "outside the allowed folders"},
		{name: "traversal into a sibling", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/a/../../agent-workspace-evil/x"`), reason: "outside"},
		{name: "sibling with the same start", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace-evil/x"`), reason: "outside"},
		{name: "parent folder", agent: "bot", tool: "fs__write_file", args: write(`"/tmp"`), reason: "outside"},
		{name: "JSON-escaped dots", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/\u002e\u002e/\u002e\u002e/etc/passwd"`), reason: "outside"},
		{name: "JSON-escaped slash", agent: "bot", tool: "fs__write_file", args: write(`"\/etc\/passwd"`), reason: "outside"},
		{name: "relative path", agent: "bot", tool: "fs__write_file", args: write(`"tmp/agent-workspace/x"`), reason: "absolute"},
		{name: "dot relative", agent: "bot", tool: "fs__write_file", args: write(`"./x"`), reason: "absolute"},
		{name: "empty path", agent: "bot", tool: "fs__write_file", args: write(`""`), reason: "absolute"},
		{name: "null byte", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/x\u0000/../../../etc"`), reason: "null byte"},
		{name: "backslash separators", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/..\\..\\etc"`), reason: "backslash"},
		{name: "case trick in path", agent: "bot", tool: "fs__write_file", args: write(`"/TMP/agent-workspace/x"`), reason: "outside"},

		// Shape of the arguments.
		{name: "case trick in argument name", agent: "bot", tool: "fs__write_file", args: `{"Path":"/etc/passwd","path":"/tmp/agent-workspace/x","content":"hi"}`, reason: `"Path" is not allowed`},
		{name: "repeated argument", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","path":"/etc/passwd","content":"hi"}`, reason: "more than once"},
		{name: "missing path", agent: "bot", tool: "fs__write_file", args: `{"content":"hi"}`, reason: `"path" is missing`},
		{name: "no arguments", agent: "bot", tool: "fs__write_file", args: "", reason: "missing"},
		{name: "null arguments", agent: "bot", tool: "fs__write_file", args: "null", reason: "missing"},
		{name: "path is a list", agent: "bot", tool: "fs__write_file", args: write(`["/tmp/agent-workspace/x"]`), reason: "must be a string"},
		{name: "path is null", agent: "bot", tool: "fs__write_file", args: write(`null`), reason: "must be a string"},
		{name: "unlisted argument", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":"hi","base_dir":"/"}`, reason: `"base_dir" is not allowed`},
		{name: "listed free argument", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":"hi","mode":"append"}`, allow: true},
		{name: "free argument takes any type", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":"hi","mode":{"a":[1]}}`, allow: true},
		{name: "arguments not an object", agent: "bot", tool: "fs__write_file", args: `["/tmp/agent-workspace/x"]`, reason: "must be an object"},
		{name: "arguments a string", agent: "bot", tool: "fs__write_file", args: `"x"`, reason: "must be an object"},
		{name: "broken JSON", agent: "bot", tool: "fs__write_file", args: `{"path":`, reason: "not valid JSON"},
		{name: "data after the object", agent: "bot", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/x"`) + `{}`, reason: "after the object"},
		{name: "invalid UTF-8", agent: "bot", tool: "fs__write_file", args: write("\"/tmp/agent-workspace/\xff\""), reason: "UTF-8"},

		// Size limits.
		{name: "content at the limit", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":"12345678901234567890"}`, allow: true},
		{name: "content over the limit", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":"123456789012345678901"}`, reason: "longer than 20 bytes"},
		{name: "escapes count as decoded bytes", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":"\u0041\u0041\u0041\u0041\u0041\u0041"}`, allow: true},
		{name: "non-string counts its JSON text", agent: "bot", tool: "fs__write_file", args: `{"path":"/tmp/agent-workspace/x","content":[1,2,3,4,5,6,7,8,9,10]}`, reason: "longer than 20 bytes"},

		// Deny patterns, matched without regard to case.
		{name: "plain query", agent: "bot", tool: "db__query", args: `{"sql":"SELECT * FROM users"}`, allow: true},
		{name: "word boundary", agent: "bot", tool: "db__query", args: `{"sql":"SELECT * FROM dropdown_options"}`, allow: true},
		{name: "drop", agent: "bot", tool: "db__query", args: `{"sql":"drop table users"}`, reason: "denied pattern"},
		{name: "DROP upper case", agent: "bot", tool: "db__query", args: `{"sql":"DROP TABLE users"}`, reason: "denied pattern"},
		{name: "mixed case", agent: "bot", tool: "db__query", args: `{"sql":"DrOp TABLE users"}`, reason: "denied pattern"},
		{name: "second statement", agent: "bot", tool: "db__query", args: `{"sql":"SELECT 1;\nTRUNCATE users"}`, reason: "denied pattern"},
		{name: "JSON-escaped keyword", agent: "bot", tool: "db__query", args: `{"sql":"\u0044ROP TABLE users"}`, reason: "denied pattern"},
		{name: "sql missing", agent: "bot", tool: "db__query", args: `{}`, reason: "missing"},
		// A known gap of blocklists: SQL comments split the keyword. The
		// real protection is a read-only database user. This case keeps the
		// gap visible; if it ever starts failing, update the README.
		{name: "known gap: comment inside keyword", agent: "bot", tool: "db__query", args: `{"sql":"DR/**/OP TABLE users"}`, allow: true},

		// Rules and roles.
		{name: "rule without args allows anything", agent: "bot", tool: "fs__read_file", args: `{"path":"/etc/passwd","extra":1}`, allow: true},
		{name: "rule without args ignores bad JSON", agent: "bot", tool: "fs__read_file", args: `not json`, allow: true},
		{name: "second role allows other folder", agent: "both", tool: "fs__write_file", args: `{"path":"/var/other/x"}`, allow: true},
		{name: "first role still works", agent: "both", tool: "fs__write_file", args: write(`"/tmp/agent-workspace/x"`), allow: true},
		{name: "no role allows the folder", agent: "both", tool: "fs__write_file", args: write(`"/etc/passwd"`), reason: `role "other-writer"`},
		{name: "empty args section allows no arguments", agent: "pinger", tool: "fs__ping", args: "", allow: true},
		{name: "empty args section allows empty object", agent: "pinger", tool: "fs__ping", args: `{}`, allow: true},
		{name: "empty args section denies any argument", agent: "pinger", tool: "fs__ping", args: `{"x":1}`, reason: `"x" is not allowed`},
		{name: "tool not allowed", agent: "bot", tool: "fs__delete", args: `{}`, reason: "no role"},
		{name: "agent not in policy", agent: "stranger", tool: "fs__read_file", args: `{}`, reason: "not in the policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.args != "" {
				raw = json.RawMessage(tt.args)
			}
			d := p.AllowCall(tt.agent, tt.tool, raw)
			if d.Allowed != tt.allow {
				t.Fatalf("AllowCall(%q, %q, %s) = %+v, want allowed=%v", tt.agent, tt.tool, tt.args, d, tt.allow)
			}
			if !tt.allow && !strings.Contains(d.Reason, tt.reason) {
				t.Errorf("reason %q does not mention %q", d.Reason, tt.reason)
			}
			if d.Allowed && d.Rule == "" {
				t.Error("allowed decision names no rule")
			}
		})
	}
}

// Reasons end up in logs, so they must never carry argument values.
func TestAllowCallReasonsHideValues(t *testing.T) {
	p := mustParse(t, argsPolicy)
	const secret = "sk-SECRET-123"
	calls := []struct{ tool, args string }{
		{"fs__write_file", `{"path":"/etc/` + secret + `","content":"hi"}`},
		{"fs__write_file", `{"path":"` + secret + `","content":"hi"}`},
		{"fs__write_file", `{"path":"/tmp/agent-workspace/x","content":"` + secret + `-and-more-text"}`},
		{"db__query", `{"sql":"DROP TABLE ` + secret + `"}`},
	}
	for _, c := range calls {
		d := p.AllowCall("bot", c.tool, json.RawMessage(c.args))
		if d.Allowed {
			t.Fatalf("AllowCall(%s) allowed, want denied", c.args)
		}
		if strings.Contains(d.Reason, secret) {
			t.Errorf("reason %q leaks the argument value", d.Reason)
		}
	}
}

// tools/list shows a tool when some rule names it, whatever its argument
// checks; the checks run on the call.
func TestAllowToolIgnoresArgChecks(t *testing.T) {
	p := mustParse(t, argsPolicy)
	for _, c := range []struct{ agent, tool string }{
		{"bot", "fs__write_file"},
		{"bot", "db__query"},
		{"pinger", "fs__ping"},
	} {
		if !p.AllowTool(c.agent, c.tool).Allowed {
			t.Errorf("AllowTool(%q, %q) denied, want allowed", c.agent, c.tool)
		}
	}
}

func TestParseRejectsBadArgRules(t *testing.T) {
	rule := func(args string) string {
		return "roles:\n  r:\n    allow:\n      - tool: fs__write_file\n        args:\n" + args + "agents:\n  a: [r]\n"
	}
	tests := []struct {
		name    string
		text    string
		wantErr []string
	}{
		{name: "args on wildcard", text: "roles:\n  r:\n    allow:\n      - tool: fs__*\n        args:\n          path: {}\nagents:\n  a: [r]\n", wantErr: []string{`"fs__*"`, "args are not allowed"}},
		{name: "unknown check", text: rule("          path: { path_prefixes: [/tmp] }\n"), wantErr: []string{"parse", "path_prefixes"}},
		{name: "relative prefix", text: rule("          path: { path_prefix: [tmp/ws] }\n"), wantErr: []string{`argument "path"`, "absolute"}},
		{name: "root prefix", text: rule("          path: { path_prefix: [/] }\n"), wantErr: []string{"allows every path"}},
		{name: "trailing slash", text: rule("          path: { path_prefix: [/tmp/ws/] }\n"), wantErr: []string{"not clean", `"/tmp/ws"`}},
		{name: "dot-dot in prefix", text: rule("          path: { path_prefix: [/tmp/ws/../etc] }\n"), wantErr: []string{"not clean"}},
		{name: "backslash in prefix", text: rule("          path: { path_prefix: ['/tmp\\ws'] }\n"), wantErr: []string{"backslash"}},
		{name: "empty prefix list", text: rule("          path: { path_prefix: [] }\n"), wantErr: []string{"path_prefix is empty"}},
		{name: "bad regex", text: rule("          sql: { deny_patterns: ['('] }\n"), wantErr: []string{`argument "sql"`, `"("`}},
		{name: "empty pattern", text: rule("          sql: { deny_patterns: [''] }\n"), wantErr: []string{"every value"}},
		{name: "zero max length", text: rule("          content: { max_length: 0 }\n"), wantErr: []string{"positive"}},
		{name: "negative max length", text: rule("          content: { max_length: -5 }\n"), wantErr: []string{"positive"}},
		{name: "max length not a number", text: rule("          content: { max_length: big }\n"), wantErr: []string{"parse"}},
		{name: "empty argument name", text: rule("          '': {}\n"), wantErr: []string{"argument name is empty"}},
		{
			name:    "all problems together",
			text:    rule("          path: { path_prefix: [rel] }\n          sql: { deny_patterns: ['['] }\n"),
			wantErr: []string{`argument "path"`, `argument "sql"`},
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
