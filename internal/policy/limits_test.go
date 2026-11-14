package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/ratelimit"
)

const limitsPolicy = `
roles:
  r:
    allow:
      - tool: email__*
      - tool: search__*
agents:
  a: [r]
limits:
  default: { rate: 60/minute, burst: 10 }
  tools:
    - tool: search__*
      rate: 100/minute
      burst: 20
    - tool: email__*
      rate: 1/hour
      burst: 1
    - tool: email__send
      rate: 5/minute
      burst: 2
`

func TestLimitFor(t *testing.T) {
	p := mustParse(t, limitsPolicy)
	tests := []struct {
		tool  string
		want  ratelimit.Limit
		entry string
	}{
		{tool: "email__send", want: ratelimit.Limit{Count: 5, Per: time.Minute, Burst: 2}, entry: "email__send"},
		// email__* comes before email__send in the file, but email__send still
		// wins for its own tool: exact beats wildcard, whatever the order.
		{tool: "email__delete", want: ratelimit.Limit{Count: 1, Per: time.Hour, Burst: 1}, entry: "email__*"},
		{tool: "search__web", want: ratelimit.Limit{Count: 100, Per: time.Minute, Burst: 20}, entry: "search__*"},
		{tool: "fs__read", want: ratelimit.Limit{Count: 60, Per: time.Minute, Burst: 10}, entry: "default"},
		{tool: "email__", want: ratelimit.Limit{Count: 60, Per: time.Minute, Burst: 10}, entry: "default"},
		{tool: "email-archive__x", want: ratelimit.Limit{Count: 60, Per: time.Minute, Burst: 10}, entry: "default"},
		{tool: "EMAIL__send", want: ratelimit.Limit{Count: 60, Per: time.Minute, Burst: 10}, entry: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got, entry := p.LimitFor(tt.tool)
			if got != tt.want || entry != tt.entry {
				t.Errorf("LimitFor(%q) = %v from %q, want %v from %q", tt.tool, got, entry, tt.want, tt.entry)
			}
		})
	}
}

func TestParseRejectsBadLimits(t *testing.T) {
	const head = "roles:\n  r:\n    allow:\n      - tool: fs__x\nagents:\n  a: [r]\n"
	tests := []struct {
		name    string
		limits  string
		wantErr []string
	}{
		{name: "no limits section", limits: "", wantErr: []string{"limits section is required"}},
		{name: "no default", limits: "limits:\n  tools:\n    - { tool: fs__x, rate: 5/minute, burst: 1 }\n", wantErr: []string{"limits.default is required"}},
		{name: "default without rate", limits: "limits:\n  default: { burst: 1 }\n", wantErr: []string{"limits.default", "rate is required"}},
		{name: "default without burst", limits: "limits:\n  default: { rate: 5/minute }\n", wantErr: []string{"limits.default", "burst is required"}},
		{name: "bad rate", limits: "limits:\n  default: { rate: 5/min, burst: 1 }\n", wantErr: []string{"limits.default", `"5/min"`}},
		{name: "zero rate", limits: "limits:\n  default: { rate: 0/minute, burst: 1 }\n", wantErr: []string{"limits.default", "count"}},
		{name: "negative burst", limits: "limits:\n  default: { rate: 5/minute, burst: -1 }\n", wantErr: []string{"limits.default", "burst"}},
		{name: "huge burst", limits: "limits:\n  default: { rate: 5/minute, burst: 2000000 }\n", wantErr: []string{"limits.default", "burst"}},
		{name: "unknown field", limits: "limits:\n  default: { rate: 5/minute, burst: 1, per_agent: true }\n", wantErr: []string{"parse", "per_agent"}},
		{name: "tool entry without tool", limits: "limits:\n  default: { rate: 5/minute, burst: 1 }\n  tools:\n    - { rate: 5/minute, burst: 1 }\n", wantErr: []string{"limits.tools[0]", "tool is required"}},
		{name: "partial wildcard", limits: "limits:\n  default: { rate: 5/minute, burst: 1 }\n  tools:\n    - { tool: 'fs__read_*', rate: 5/minute, burst: 1 }\n", wantErr: []string{"limits.tools[0]", `"fs__read_*"`}},
		{name: "tool entry without burst", limits: "limits:\n  default: { rate: 5/minute, burst: 1 }\n  tools:\n    - { tool: fs__x, rate: 5/minute }\n", wantErr: []string{"limits.tools[0]", "burst is required"}},
		{name: "tool listed twice", limits: "limits:\n  default: { rate: 5/minute, burst: 1 }\n  tools:\n    - { tool: fs__x, rate: 5/minute, burst: 1 }\n    - { tool: fs__x, rate: 9/minute, burst: 1 }\n", wantErr: []string{"limits.tools[1]", "listed twice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(head + tt.limits))
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

func TestCheckNamesCoversLimits(t *testing.T) {
	p := mustParse(t, limitsPolicy)
	if err := p.CheckNames([]string{"email", "search"}, []string{"a"}); err != nil {
		t.Fatalf("CheckNames() = %v, want nil", err)
	}
	// A limit for a misspelled upstream would silently never apply.
	err := p.CheckNames([]string{"email"}, []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "limits.tools[0]") || !strings.Contains(err.Error(), `upstream "search"`) {
		t.Fatalf("CheckNames() = %v, want an error about limits.tools[0] naming search", err)
	}
}

func TestLoadExamplePolicyLimits(t *testing.T) {
	p, err := Load("../../testdata/config/example-policy.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, entry := p.LimitFor("fake__count"); entry != "fake__count" {
		t.Errorf("fake__count uses %q, want its own entry", entry)
	}
	if _, entry := p.LimitFor("fake__echo"); entry != "default" {
		t.Errorf("fake__echo uses %q, want the default", entry)
	}
}
