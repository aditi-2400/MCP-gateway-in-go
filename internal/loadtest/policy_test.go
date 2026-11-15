package loadtest

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/policy"
)

// loadWrittenPolicy writes the gateway files with the named policy and
// loads the policy the way the gateway does.
func loadWrittenPolicy(t *testing.T, name string) *policy.Policy {
	t.Helper()
	args, err := WriteGatewayFiles(t.TempDir(), fakeUpstreamBin, "0000", name)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-policy")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("args %q have no -policy", args)
	}
	p, err := policy.Load(args[i+1])
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckNames([]string{UpstreamName}, []string{Agent}); err != nil {
		t.Fatal(err)
	}
	return p
}

func echoArgs(text string) json.RawMessage {
	return json.RawMessage(`{"text":` + strconv.Quote(text) + `}`)
}

// The benchmark's own call has to pass every policy, or the run would
// measure denials.
func TestPoliciesAllowTheBenchmarkCall(t *testing.T) {
	for _, name := range Policies {
		p := loadWrittenPolicy(t, name)
		if d := p.AllowCall(Agent, UpstreamName+"__"+Tool, echoArgs(Text)); !d.Allowed {
			t.Errorf("%s policy denies the benchmark call: %s", name, d.Reason)
		}
	}
}

// The realistic policy really runs its checks: values a real policy would
// stop are denied. The minimal one has no checks, so it allows them.
func TestOnlyTheRealisticPolicyChecksArguments(t *testing.T) {
	realistic := loadWrittenPolicy(t, PolicyRealistic)
	minimal := loadWrittenPolicy(t, PolicyMinimal)
	for _, text := range []string{
		"/etc/passwd",                          // outside the folders
		"/tmp/agent-workspace/../../etc/hosts", // escapes after cleaning
		"/tmp/agent-workspace/.env",            // denied pattern
		"/tmp/agent-workspace/DROP  TABLE x",   // denied pattern, any case
		"relative/path",                        // not absolute
	} {
		if d := realistic.AllowCall(Agent, UpstreamName+"__"+Tool, echoArgs(text)); d.Allowed {
			t.Errorf("realistic policy allows %q", text)
		}
		if d := minimal.AllowCall(Agent, UpstreamName+"__"+Tool, echoArgs(text)); !d.Allowed {
			t.Errorf("minimal policy denies %q: %s", text, d.Reason)
		}
	}
}

func TestWriteGatewayFilesRejectsAnUnknownPolicy(t *testing.T) {
	if _, err := WriteGatewayFiles(t.TempDir(), fakeUpstreamBin, "0000", "strict"); err == nil {
		t.Error("no error for an unknown policy")
	}
}
