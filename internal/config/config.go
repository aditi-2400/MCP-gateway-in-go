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

// Config is the whole config file.
type Config struct {
	Upstreams []Upstream `yaml:"upstreams"`
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

func (u Upstream) validate() []error {
	var errs []error
	switch {
	case u.Name == "":
		errs = append(errs, errors.New("name is required"))
	case len(u.Name) > maxNameLength:
		errs = append(errs, fmt.Errorf("name %q must be at most %d characters", u.Name, maxNameLength))
	case !namePattern.MatchString(u.Name):
		errs = append(errs, fmt.Errorf("name %q must start with a lowercase letter and use only lowercase letters, digits, and '-'", u.Name))
	}
	if strings.TrimSpace(u.Command) == "" {
		errs = append(errs, errors.New("command is required"))
	}
	return errs
}
