package loadtest

import "fmt"

// Policy names for WriteGatewayFiles.
const (
	// PolicyMinimal is the smallest policy that allows the calls: every
	// tool of the upstream, no argument checks.
	PolicyMinimal = "minimal"
	// PolicyRealistic looks like a policy someone would run: two roles,
	// rules for other tools ahead of the one that matches, and argument
	// checks on Text with a length limit, allowed folders, and patterns.
	PolicyRealistic = "realistic"
)

// Policies lists every policy name, in the order to report them.
var Policies = []string{PolicyMinimal, PolicyRealistic}

// noLimit is a rate limit too high to reach, so a run measures the limiter's
// work without being stopped by it.
const noLimit = "{ rate: 1000000/second, burst: 1000000 }"

var policies = map[string]string{
	PolicyMinimal: fmt.Sprintf(`roles:
  all:
    allow:
      - tool: %[1]s__*
agents:
  %[2]s: [all]
limits:
  default: %[3]s
`, UpstreamName, Agent, noLimit),

	// The readers role comes first and has no rule for Tool, so every call
	// looks past its rules before reaching the writers role.
	PolicyRealistic: fmt.Sprintf(`roles:
  readers:
    allow:
      - tool: %[1]s__lookup
        args:
          region: { max_length: 32, deny_patterns: ['[^a-z0-9-]'] }
          limit: { max_length: 4 }
          verbose: {}
      - tool: %[1]s__count
        args:
          n: { max_length: 3 }
  writers:
    allow:
      - tool: %[1]s__add
        args:
          a: { max_length: 20 }
          b: { max_length: 20 }
      - tool: %[1]s__%[4]s
        args:
          text:
            max_length: 4096
            path_prefix: [/tmp/agent-workspace, /srv/shared]
            deny_patterns: ['\bdrop\s+table\b', '\bpassword\b', '(^|/)\.env$', 'id_rsa']
agents:
  %[2]s: [readers, writers]
limits:
  default: %[3]s
  tools:
    - { tool: %[1]s__lookup, rate: 1000000/second, burst: 1000000 }
    - { tool: %[1]s__count, rate: 1000000/second, burst: 1000000 }
    - { tool: %[1]s__%[4]s, rate: 1000000/second, burst: 1000000 }
`, UpstreamName, Agent, noLimit, Tool),
}
