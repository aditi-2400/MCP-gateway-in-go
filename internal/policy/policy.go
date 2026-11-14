// Package policy decides which agent may call which tool.
//
// The rule is default deny: a call is allowed only if one of the agent's
// roles has a rule that matches the tool. An agent with no roles, an agent
// missing from the policy, or a tool no rule mentions is always denied.
//
// A rule can also check arguments: folders a path must stay inside,
// patterns a value must not match, and length limits. Arguments a rule
// with checks does not list are denied too.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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
// (github__*). A rule without Args allows any arguments. A rule with Args
// allows only the arguments it lists, and each one must pass its checks.
type Rule struct {
	Tool string             `yaml:"tool"`
	Args map[string]ArgRule `yaml:"args"`
}

// ArgRule holds the checks for one top-level argument. An empty ArgRule
// (written `mode: {}`) allows the argument with any value, or leaving it
// out. An ArgRule with any check makes the argument required, because the
// upstream's default for a missing argument could be anything.
type ArgRule struct {
	// PathPrefix lists folders the value must be inside, after cleaning.
	PathPrefix []string `yaml:"path_prefix"`
	// DenyPatterns are Go regular expressions, matched without regard to
	// case. A value matching any of them is denied.
	DenyPatterns []string `yaml:"deny_patterns"`
	// MaxLength caps a string's length in bytes. For any other JSON value
	// it caps the length of the value's JSON text.
	MaxLength *int `yaml:"max_length"`

	denyRegexps []*regexp.Regexp // compiled from DenyPatterns by validate
}

func (r ArgRule) hasChecks() bool {
	return len(r.PathPrefix) > 0 || len(r.denyRegexps) > 0 || r.MaxLength != nil
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
			where := fmt.Sprintf("role %q: allow[%d]", name, i)
			if err := checkToolPattern(rule.Tool); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
			}
			errs = append(errs, compileArgs(where, rule)...)
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

// compileArgs checks a rule's argument section and compiles its deny
// patterns. Writing back into rule.Args updates the policy, because a map
// is shared between copies of the Rule.
func compileArgs(where string, rule Rule) []error {
	if rule.Args == nil {
		return nil
	}
	if strings.HasSuffix(rule.Tool, separator+wildcard) {
		return []error{fmt.Errorf("%s: args are not allowed on %q, since each tool has its own arguments", where, rule.Tool)}
	}
	var errs []error
	for _, argName := range sortedKeys(rule.Args) {
		ar := rule.Args[argName]
		at := fmt.Sprintf("%s: argument %q", where, argName)
		if argName == "" {
			errs = append(errs, fmt.Errorf("%s: argument name is empty", where))
		}
		// An empty list would mean "no path check", which reads like "no
		// path allowed". Refuse it so the file can't mean the opposite of
		// what it seems to say.
		if ar.PathPrefix != nil && len(ar.PathPrefix) == 0 {
			errs = append(errs, fmt.Errorf("%s: path_prefix is empty", at))
		}
		for _, prefix := range ar.PathPrefix {
			if err := checkPathPrefix(prefix); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", at, err))
			}
		}
		ar.denyRegexps = nil
		for _, pattern := range ar.DenyPatterns {
			if pattern == "" {
				errs = append(errs, fmt.Errorf("%s: empty deny pattern would match every value", at))
				continue
			}
			re, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: deny pattern %q: %w", at, pattern, err))
				continue
			}
			ar.denyRegexps = append(ar.denyRegexps, re)
		}
		if ar.MaxLength != nil && *ar.MaxLength <= 0 {
			errs = append(errs, fmt.Errorf("%s: max_length must be positive", at))
		}
		rule.Args[argName] = ar
	}
	return errs
}

// checkPathPrefix wants an absolute, already clean folder, so the prefix
// compared at call time is exactly the one written in the file.
func checkPathPrefix(prefix string) error {
	switch {
	case !strings.HasPrefix(prefix, "/"):
		return fmt.Errorf("path_prefix %q must be absolute", prefix)
	case prefix == "/":
		return errors.New(`path_prefix "/" allows every path; remove path_prefix instead`)
	case strings.ContainsAny(prefix, "\x00\\"):
		return fmt.Errorf("path_prefix %q contains a null byte or backslash", prefix)
	case path.Clean(prefix) != prefix:
		return fmt.Errorf("path_prefix %q is not clean; write %q", prefix, path.Clean(prefix))
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

// AllowTool decides whether agent may call tool with some arguments. It
// ignores argument checks, so it answers "should this agent see the tool in
// tools/list". Use AllowCall for an actual call.
func (p *Policy) AllowTool(agent, tool string) Decision {
	roles, ok := p.Agents[agent]
	if !ok {
		return Decision{Reason: fmt.Sprintf("agent %q is not in the policy", agent)}
	}
	for _, roleName := range roles {
		for _, rule := range p.Roles[roleName].Allow {
			if matches(rule.Tool, tool) {
				return allowed(roleName, rule)
			}
		}
	}
	return Decision{Reason: fmt.Sprintf("no role of agent %q allows tool %q", agent, tool)}
}

// AllowCall decides whether agent may call tool with these arguments.
// arguments is the raw "arguments" value of tools/call; nil or null means
// none were sent. The call is allowed if any matching rule passes.
//
// Reasons name arguments but never include their values, because values can
// hold secrets and reasons end up in logs.
func (p *Policy) AllowCall(agent, tool string, arguments json.RawMessage) Decision {
	roles, ok := p.Agents[agent]
	if !ok {
		return Decision{Reason: fmt.Sprintf("agent %q is not in the policy", agent)}
	}
	var (
		args     map[string]json.RawMessage
		parseErr error
		parsed   bool
		failures []string
	)
	for _, roleName := range roles {
		for _, rule := range p.Roles[roleName].Allow {
			if !matches(rule.Tool, tool) {
				continue
			}
			if rule.Args == nil {
				return allowed(roleName, rule)
			}
			// Parse only when a rule needs the arguments, and only once.
			if !parsed {
				args, parseErr = parseArguments(arguments)
				parsed = true
			}
			err := parseErr
			if err == nil {
				err = checkArgs(rule.Args, args)
			}
			if err == nil {
				return allowed(roleName, rule)
			}
			failures = append(failures, fmt.Sprintf("role %q rule %s: %v", roleName, rule.Tool, err))
		}
	}
	if len(failures) == 0 {
		return Decision{Reason: fmt.Sprintf("no role of agent %q allows tool %q", agent, tool)}
	}
	return Decision{Reason: strings.Join(failures, "; ")}
}

func allowed(roleName string, rule Rule) Decision {
	return Decision{
		Allowed: true,
		Rule:    roleName + ": " + rule.Tool,
		Reason:  fmt.Sprintf("role %q allows %s", roleName, rule.Tool),
	}
}

// parseArguments reads the arguments object by hand for two reasons. A
// repeated key is refused, because the gateway and the upstream might keep
// different copies (Go keeps the last; some parsers keep the first), and
// then the gateway would check one value while the upstream used the other.
// And invalid UTF-8 is refused, because Go would quietly replace it, so the
// checked string would differ from the bytes forwarded.
func parseArguments(raw json.RawMessage) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	args := make(map[string]json.RawMessage)
	if len(raw) == 0 || string(raw) == "null" {
		return args, nil
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("arguments are not valid UTF-8")
	}
	notJSON := errors.New("arguments are not valid JSON")
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, notJSON
	}
	if tok != json.Delim('{') {
		return nil, errors.New("arguments must be an object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, notJSON
		}
		key, ok := tok.(string)
		if !ok {
			return nil, notJSON
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, notJSON
		}
		if _, dup := args[key]; dup {
			return nil, fmt.Errorf("argument %q appears more than once", key)
		}
		args[key] = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, notJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("arguments have data after the object")
	}
	return args, nil
}

// checkArgs applies a rule's argument checks. Names are compared exactly,
// so "Path" is a different, unlisted argument from "path".
func checkArgs(rules map[string]ArgRule, args map[string]json.RawMessage) error {
	for _, name := range sortedKeys(args) {
		if _, ok := rules[name]; !ok {
			return fmt.Errorf("argument %q is not allowed", name)
		}
	}
	for _, name := range sortedKeys(rules) {
		r := rules[name]
		value, ok := args[name]
		if !ok {
			if r.hasChecks() {
				return fmt.Errorf("argument %q is missing", name)
			}
			continue
		}
		if err := r.check(value); err != nil {
			return fmt.Errorf("argument %q %w", name, err)
		}
	}
	return nil
}

// check tests one value. Errors finish the sentence "argument X ...".
func (r ArgRule) check(value json.RawMessage) error {
	var s string
	isString := len(value) > 0 && value[0] == '"' && json.Unmarshal(value, &s) == nil
	if !isString && (len(r.PathPrefix) > 0 || len(r.denyRegexps) > 0) {
		return errors.New("must be a string")
	}
	// Length goes first, so the regular expressions only ever see bounded
	// input.
	if r.MaxLength != nil {
		n := len(value)
		if isString {
			n = len(s)
		}
		if n > *r.MaxLength {
			return fmt.Errorf("is longer than %d bytes", *r.MaxLength)
		}
	}
	if len(r.PathPrefix) > 0 {
		if err := checkPath(s, r.PathPrefix); err != nil {
			return err
		}
	}
	for _, re := range r.denyRegexps {
		if re.MatchString(s) {
			return fmt.Errorf("matches denied pattern %q", re.String())
		}
	}
	return nil
}

// checkPath cleans the path lexically and then compares whole folder
// names, so /tmp/ws/../../etc becomes /etc (outside) and /tmp/ws-evil is
// not inside /tmp/ws. It can't follow symlinks: the files live on the
// upstream's machine, which the gateway can't see.
func checkPath(value string, prefixes []string) error {
	// A null byte can cut a path short in C-based code, and a backslash is
	// a separator on Windows that path.Clean does not know about.
	if strings.ContainsAny(value, "\x00\\") {
		return errors.New("contains a null byte or backslash")
	}
	if !strings.HasPrefix(value, "/") {
		return errors.New("must be an absolute path")
	}
	clean := path.Clean(value)
	for _, prefix := range prefixes {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return nil
		}
	}
	return errors.New("is outside the allowed folders")
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
