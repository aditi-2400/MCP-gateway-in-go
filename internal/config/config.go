// Package config loads and validates the gateway's YAML config file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

// maxNameLength keeps prefixed tool names (name + "__" + tool) well under
// the 128 characters MCP suggests for tool names.
const maxNameLength = 32

// Upstream names become tool prefixes, as in fs__read_file. Underscores are
// not allowed, so the "__" separator can never be ambiguous.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// keyHashPattern is a SHA-256 in lowercase hex, as `gateway keygen` prints.
var keyHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Config is the whole config file.
type Config struct {
	Upstreams []Upstream `yaml:"upstreams"`
	// Agents are the callers allowed to use the gateway. At least one is
	// required: with no agents, nobody could authenticate.
	Agents []Agent `yaml:"agents"`
}

// Agent is one caller, identified by an API key. Only the key's SHA-256 is
// stored, so the config file never holds a usable secret.
type Agent struct {
	Name      string `yaml:"name"`
	KeySHA256 string `yaml:"key_sha256"`
}

// Upstream is one MCP server the gateway starts and connects to over stdio.
type Upstream struct {
	// Name identifies the upstream and prefixes its tool names.
	Name string `yaml:"name"`
	// Command is the program to run. It is run directly, not through a shell.
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func parse(data []byte) (*Config, error) {
	var cfg Config
	// Unknown fields are errors, so a typo like "comand" is caught instead of
	// silently ignored.
	if err := yaml.Load(data, &cfg, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate reports every problem at once, so the whole file can be fixed in
// one pass.
func (c *Config) validate() error {
	return errors.Join(c.validateUpstreams(), c.validateAgents())
}

func (c *Config) validateUpstreams() error {
	if len(c.Upstreams) == 0 {
		return errors.New("no upstreams configured")
	}

	var errs []error
	firstUse := make(map[string]int) // name -> index of the upstream that uses it
	for i, up := range c.Upstreams {
		for _, err := range up.validate() {
			errs = append(errs, fmt.Errorf("upstreams[%d]: %w", i, err))
		}
		if up.Name == "" {
			continue
		}
		if j, ok := firstUse[up.Name]; ok {
			errs = append(errs, fmt.Errorf("upstreams[%d]: name %q is already used by upstreams[%d]", i, up.Name, j))
			continue
		}
		firstUse[up.Name] = i
	}
	return errors.Join(errs...)
}

func (c *Config) validateAgents() error {
	if len(c.Agents) == 0 {
		return errors.New("no agents configured; add one with `gateway keygen -agent NAME`")
	}

	var errs []error
	nameUse := make(map[string]int)
	keyUse := make(map[string]int)
	for i, a := range c.Agents {
		for _, err := range a.validate() {
			errs = append(errs, fmt.Errorf("agents[%d]: %w", i, err))
		}
		if j, ok := nameUse[a.Name]; ok && a.Name != "" {
			errs = append(errs, fmt.Errorf("agents[%d]: name %q is already used by agents[%d]", i, a.Name, j))
		} else {
			nameUse[a.Name] = i
		}
		if j, ok := keyUse[a.KeySHA256]; ok && a.KeySHA256 != "" {
			errs = append(errs, fmt.Errorf("agents[%d]: key_sha256 is the same as agents[%d]; each agent needs its own key", i, j))
		} else {
			keyUse[a.KeySHA256] = i
		}
	}
	return errors.Join(errs...)
}

// validate never puts key_sha256 in an error, in case someone pasted a raw
// key there and the error ends up in a log.
func (a Agent) validate() []error {
	var errs []error
	if err := CheckName(a.Name); err != nil {
		errs = append(errs, err)
	}
	switch {
	case a.KeySHA256 == "":
		errs = append(errs, errors.New("key_sha256 is required"))
	case strings.HasPrefix(a.KeySHA256, "mcpgw_"):
		errs = append(errs, errors.New("key_sha256 holds a raw API key; store only its hash (see `gateway keygen`), and replace that key since it is now in a file"))
	case !keyHashPattern.MatchString(a.KeySHA256):
		errs = append(errs, errors.New("key_sha256 must be 64 lowercase hex characters (see `gateway keygen`)"))
	}
	return errs
}

// CheckName applies the rule for upstream and agent names: a lowercase
// letter, then lowercase letters, digits, and '-', at most 32 characters.
func CheckName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required")
	case len(name) > maxNameLength:
		return fmt.Errorf("name %q must be at most %d characters", name, maxNameLength)
	case !namePattern.MatchString(name):
		return fmt.Errorf("name %q must start with a lowercase letter and use only lowercase letters, digits, and '-'", name)
	}
	return nil
}

func (u Upstream) validate() []error {
	var errs []error
	if err := CheckName(u.Name); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(u.Command) == "" {
		errs = append(errs, errors.New("command is required"))
	}
	return errs
}
