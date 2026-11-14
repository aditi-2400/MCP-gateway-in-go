package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/ratelimit"
)

// Limits is the limits: section. Default is required, so a tool nobody
// listed is still limited.
type Limits struct {
	Default *LimitSpec  `yaml:"default"`
	Tools   []ToolLimit `yaml:"tools"`
}

// LimitSpec is one rate as written in the file: rate "5/minute", burst 2.
type LimitSpec struct {
	Rate  string `yaml:"rate"`
	Burst int    `yaml:"burst"`

	limit ratelimit.Limit // filled in by validate
}

// ToolLimit sets the limit for one tool (email__send) or for each tool of
// one upstream (search__*). Each agent has its own bucket per tool, so
// search__* at 100/minute lets an agent call every search tool 100 times a
// minute, each counted on its own.
type ToolLimit struct {
	Tool  string `yaml:"tool"`
	Rate  string `yaml:"rate"`
	Burst int    `yaml:"burst"`

	limit ratelimit.Limit // filled in by validate
}

// compile checks the spec and stores the parsed limit. Errors finish the
// sentence "<where>: ...".
func compileLimit(rate string, burst int) (ratelimit.Limit, error) {
	if rate == "" {
		return ratelimit.Limit{}, errors.New("rate is required, like 5/minute")
	}
	if burst == 0 {
		return ratelimit.Limit{}, errors.New("burst is required: how many calls may come at once")
	}
	count, per, err := ratelimit.ParseRate(rate)
	if err != nil {
		return ratelimit.Limit{}, err
	}
	lim := ratelimit.Limit{Count: count, Per: per, Burst: burst}
	if err := lim.Validate(); err != nil {
		return ratelimit.Limit{}, err
	}
	return lim, nil
}

func (p *Policy) validateLimits() []error {
	if p.Limits == nil {
		return []error{errors.New("limits section is required, with at least a default limit")}
	}
	var errs []error
	if p.Limits.Default == nil {
		errs = append(errs, errors.New("limits.default is required; it applies to every tool without its own entry"))
	} else {
		lim, err := compileLimit(p.Limits.Default.Rate, p.Limits.Default.Burst)
		if err != nil {
			errs = append(errs, fmt.Errorf("limits.default: %w", err))
		}
		p.Limits.Default.limit = lim
	}
	seen := make(map[string]bool)
	for i := range p.Limits.Tools {
		tl := &p.Limits.Tools[i]
		where := fmt.Sprintf("limits.tools[%d]", i)
		if err := checkToolPattern(tl.Tool); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		if seen[tl.Tool] {
			errs = append(errs, fmt.Errorf("%s: tool %q is listed twice", where, tl.Tool))
		}
		seen[tl.Tool] = true
		lim, err := compileLimit(tl.Rate, tl.Burst)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		tl.limit = lim
	}
	return errs
}

func (p *Policy) checkLimitNames(upstreams []string) []error {
	var errs []error
	for i, tl := range p.Limits.Tools {
		upstream, _, _ := strings.Cut(tl.Tool, separator)
		if !slices.Contains(upstreams, upstream) {
			errs = append(errs, fmt.Errorf("limits.tools[%d]: tool %q names upstream %q, which is not in the config", i, tl.Tool, upstream))
		}
	}
	return errs
}

// LimitFor returns the limit for tool and the entry it came from, for logs.
// An exact entry wins over upstream__*, which wins over the default.
func (p *Policy) LimitFor(tool string) (ratelimit.Limit, string) {
	for _, tl := range p.Limits.Tools {
		if tl.Tool == tool {
			return tl.limit, tl.Tool
		}
	}
	for _, tl := range p.Limits.Tools {
		if strings.HasSuffix(tl.Tool, separator+wildcard) && matches(tl.Tool, tool) {
			return tl.limit, tl.Tool
		}
	}
	return p.Limits.Default.limit, "default"
}
