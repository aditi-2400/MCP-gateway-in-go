// Package policy decides which agent may call which tool.
//
// The rule is default deny: a call is allowed only if one of the agent's
// roles has a rule that matches the tool. An agent with no roles, an agent
// missing from the policy, or a tool no rule mentions is always denied.
package policy

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/config"
)

// separator matches router.Separator. It is repeated here so the policy
// package does not depend on the router.
const separator = "__"

// wildcard as the tool part allows every tool of one upstream: github__*.
const wildcard = "*"

// Policy is a loaded, validated policy file.
type Policy struct {
	Roles  map[string]Role     `yaml:"roles"`
	Agents map[string][]string `yaml:"agents"` // agent -> role names
}

// Role is a named set of rules.
type Role struct {
	Allow []Rule `yaml:"allow"`
}

// Rule allows one tool (fs__read_file) or every tool of one upstream
// (github__*).
type Rule struct {
	Tool string `yaml:"tool"`
}

// Decision is the answer for one call, with enough detail to log and audit.
type Decision struct {
	Allowed bool
	// Rule names the role and rule that allowed the call, like
	// "readers: github__*". Empty when denied.
	Rule string
	// Reason explains the decision in words, for logs.
	Reason string
}

// Load reads and validates the policy file at path.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	p, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	return p, nil
}

func parse(data []byte) (*Policy, error) {
	var p Policy
	// Unknown fields are errors, so "allow_all: true" or a typo like "tol"
	// can never be silently ignored.
	if err := yaml.Load(data, &p, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// validate reports every problem at once. Map keys are sorted so the
// messages come out in the same order every time.
func (p *Policy) validate() error {
	var errs []error
	for _, name := range sortedKeys(p.Roles) {
		if err := config.CheckName(name); err != nil {
			errs = append(errs, fmt.Errorf("role: %w", err))
		}
		role := p.Roles[name]
		if len(role.Allow) == 0 {
			errs = append(errs, fmt.Errorf("role %q has no rules", name))
		}
		for i, rule := range role.Allow {
			if err := checkToolPattern(rule.Tool); err != nil {
				errs = append(errs, fmt.Errorf("role %q: allow[%d]: %w", name, i, err))
			}
		}
	}

	if len(p.Agents) == 0 {
		errs = append(errs, errors.New("no agents listed; with none, every call is denied"))
	}
	for _, agent := range sortedKeys(p.Agents) {
		if err := config.CheckName(agent); err != nil {
			errs = append(errs, fmt.Errorf("agent: %w", err))
		}
		seen := make(map[string]bool)
		for _, role := range p.Agents[agent] {
			if _, ok := p.Roles[role]; !ok {
				errs = append(errs, fmt.Errorf("agent %q has unknown role %q", agent, role))
			}
			if seen[role] {
				errs = append(errs, fmt.Errorf("agent %q lists role %q twice", agent, role))
			}
			seen[role] = true
		}
	}
	return errors.Join(errs...)
}

// checkToolPattern accepts upstream__tool or upstream__*. A star anywhere
// else is rejected: patterns that span upstreams or match part of a name
// could quietly allow tools added later.
func checkToolPattern(pattern string) error {
	if pattern == "" {
		return errors.New("tool is required")
	}
	upstream, tool, found := strings.Cut(pattern, separator)
	switch {
	case !found:
		return fmt.Errorf("tool %q must look like upstream__tool or upstream__*", pattern)
	case config.CheckName(upstream) != nil:
		return fmt.Errorf("tool %q: %q is not a valid upstream name", pattern, upstream)
	case tool == "":
		return fmt.Errorf("tool %q has no tool part; use upstream__* for every tool", pattern)
	case tool != wildcard && strings.Contains(tool, wildcard):
		return fmt.Errorf("tool %q: * is only allowed as the whole tool part (upstream__*)", pattern)
	case strings.ContainsAny(tool, " \t\r\n"):
		return fmt.Errorf("tool %q contains whitespace", pattern)
	}
	return nil
}

// CheckNames compares the policy with the main config. A rule for an
// upstream that does not exist is likely a typo, and an agent with no key
// in the config could never authenticate, so both are errors. Config agents
// missing from the policy are fine: they are allowed nothing.
func (p *Policy) CheckNames(upstreams, agents []string) error {
	var errs []error
	for _, name := range sortedKeys(p.Roles) {
		for i, rule := range p.Roles[name].Allow {
			upstream, _, _ := strings.Cut(rule.Tool, separator)
			if !slices.Contains(upstreams, upstream) {
				errs = append(errs, fmt.Errorf("role %q: allow[%d]: tool %q names upstream %q, which is not in the config", name, i, rule.Tool, upstream))
			}
		}
	}
	for _, agent := range sortedKeys(p.Agents) {
		if !slices.Contains(agents, agent) {
			errs = append(errs, fmt.Errorf("agent %q is in the policy but has no key in the config", agent))
		}
	}
	return errors.Join(errs...)
}

// AllowTool decides whether agent may call tool, ignoring arguments.
func (p *Policy) AllowTool(agent, tool string) Decision {
	roles, ok := p.Agents[agent]
	if !ok {
		return Decision{Reason: fmt.Sprintf("agent %q is not in the policy", agent)}
	}
	for _, roleName := range roles {
		for _, rule := range p.Roles[roleName].Allow {
			if matches(rule.Tool, tool) {
				return Decision{
					Allowed: true,
					Rule:    roleName + ": " + rule.Tool,
					Reason:  fmt.Sprintf("role %q allows %s", roleName, rule.Tool),
				}
			}
		}
	}
	return Decision{Reason: fmt.Sprintf("no role of agent %q allows tool %q", agent, tool)}
}

// matches compares exactly, or by upstream for upstream__*. It is
// case-sensitive, like MCP tool names.
func matches(pattern, tool string) bool {
	if upstream, ok := strings.CutSuffix(pattern, separator+wildcard); ok {
		rest, found := strings.CutPrefix(tool, upstream+separator)
		return found && rest != ""
	}
	return pattern == tool
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
