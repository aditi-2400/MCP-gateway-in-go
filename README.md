# MCP Gateway in Go

A gateway that sits between AI agents and MCP servers.

Agents connect to the gateway as if it were a single MCP server. Behind it, the gateway connects to many upstream MCP servers (filesystem, database, GitHub, and so on). Every tool call goes through the gateway, so it can decide who may call which tool, how often, and with which arguments. It logs every call.

> Status: the gateway runs over stdio or Streamable HTTP in front of several upstream MCP servers, merges their tools under prefixed names, and routes each tool call to the right server. Every caller needs an API key, and a policy file decides which tools each agent can see and call, with checks on the arguments and a rate limit per agent and tool. Every tool call is recorded in a SQLite audit log, with secrets taken out of the arguments. With `-metrics`, it also serves Prometheus metrics, and with `-trace` it sends OpenTelemetry traces.

## Why

MCP gives agents direct access to real tools. Once an agent can write files or run SQL, you need one place that answers:

- Which agent made this call?
- Is it allowed to call this tool with these arguments?
- Is it calling too often?
- What happened, and can we look it up later?

The gateway is that place.

## Protocol version

The gateway targets MCP spec **2026-07-28** only. In this version there is no `initialize` handshake: every request carries its protocol version and client capabilities in `params._meta`, and servers never send requests to clients. Older clients and servers that need `initialize` (2025-11-25 and earlier) are not supported.

## How it works today

```
client ──stdin/stdout──▶ proxy ─────┐                                                   ┌──▶ upstream "fs"     (child process)
                                    ├──▶ audit ──▶ policy ──▶ ratelimit ──▶ router ─────┤
client ──HTTP POST /mcp──▶ httpfront┘                                                   └──▶ upstream "github" (child process)
```

One gateway process serves either stdio or HTTP, picked with the `-http` flag.

- **`cmd/gateway`** is the program. It reads the config file, starts every upstream, and runs the proxy on its own stdin and stdout. Logs go to stderr. It stops when the client closes stdin, on Ctrl+C or SIGTERM, or when any upstream exits.
- **`internal/config`** loads and checks the YAML config.

- **`internal/jsonrpc`** parses and validates every message as untrusted input, and reads and writes one JSON message per line, with a size limit.
- **`internal/upstream`** starts each MCP server as a child process and checks it with `server/discover` before use. If any upstream fails to start, the gateway refuses to start. It gives each request its own ID and matches each response to the caller waiting for it. On shutdown it closes each child's stdin, then sends SIGTERM, then SIGKILL.
- **`internal/proxy`** runs the client session. Each request runs on its own goroutine with a timeout, and the reply gets the client's original ID back. When a client cancels a request, the matching upstream call is cancelled.
- **`internal/httpfront`** serves Streamable HTTP for 2026-07-28. Every message is its own `POST /mcp`, with no sessions. It checks `Origin` against an allowlist, checks that the `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`, and `Mcp-Param-*` headers match the body, and answers with the spec's status codes. Closing the connection cancels the request.
- **`internal/progress`** sends upstream progress notifications back to the request that asked for them. Clients choose their own progress tokens, so two clients can pick the same one; the gateway gives each upstream request its own token and maps it back, the same way it does with request IDs. Over HTTP, progress arrives as an SSE stream.
- **`internal/router`** decides what each request does. It answers `server/discover` itself and checks the protocol version on every request. `tools/list` asks every upstream at once and returns one list with names like `fs__read_file`. If one upstream fails, its tools are left out and the rest are still listed. `tools/call` goes to the upstream named by the prefix, with the prefix removed. Other methods get "method not found" for now.
- **`internal/policy`** loads the policy file and sits in front of the router. It reads the agent that auth attached to the request, hides tools the agent may not call from `tools/list`, and checks every `tools/call` before it reaches the router. Methods it has no rule for are refused, so a method added to the router later can't skip the policy by accident.
- **`internal/audit`** comes first for every `tools/call`. It reserves room in the audit queue before anything else runs, so a call that can't be recorded is refused. When the call ends it writes one entry, whatever happened. Entries go to SQLite in the background.
- **`internal/ratelimit`** sits between the policy and the router, so calls the policy denies never use up tokens. Each agent has a token bucket per tool. A call that finds its bucket empty gets error `-31029` and never reaches the upstream.
- **`internal/metrics`** counts tool calls, denials, and latency for Prometheus. The audit layer tells it about every call it finishes.
- **`internal/tracing`** records a span for each request (outside audit, so it covers the whole time in the gateway) and one for each upstream call. It also removes the trace context a client put in `_meta` before a call reaches an upstream, since that is untrusted.
- **`internal/fakeupstream`** is a small MCP server with `echo`, `add`, and `fail` tools, used in tests.

When an upstream doesn't reply, the client gets a JSON-RPC error (`-32603`) saying "upstream timed out", "upstream unavailable", or "upstream error". The details go to the log only. Calls are never retried.

## Planned features

| Feature | What it does | Status |
|---|---|---|
| Proxy | Runs the MCP session over JSON-RPC 2.0: timeouts, cancellation, request IDs | Built |
| Multiple upstreams | Merges tools from several servers and prefixes names (`fs__read_file`) so they don't collide | Built |
| Transports | stdio (for local clients like Claude Desktop) and Streamable HTTP, with optional TLS | Built |
| Auth | API keys per agent, stored hashed | Built |
| Policy engine | YAML rules, default deny, argument checks like path prefixes and blocked SQL | Built |
| Rate limiting | Token bucket per agent and per tool | Built |
| Audit log | Append-only SQLite log of every tool call, with redacted arguments | Built |
| Observability | Prometheus metrics and OpenTelemetry traces | Built |
| Resilience | Timeouts, circuit breakers, no automatic retries on tools with side effects | Timeouts built |
| Description pinning | Hashes tool descriptions and quarantines a tool if its description changes | Planned |

## Design rules

- Default deny. If no rule allows a call, it is blocked.
- Fail closed. If the policy engine errors, the call is denied.
- No automatic retries on tool calls that could have side effects.
- Secrets in arguments are redacted before logging.
- All incoming JSON is treated as untrusted.

## Layout

```
cmd/gateway/            entry point and end-to-end tests
internal/jsonrpc/       message types, validation, newline framing, errors
internal/upstream/      start and talk to upstream servers
internal/proxy/         forward messages between client and upstream
internal/fakeupstream/  fake MCP server used in tests
internal/config/        YAML config loading and validation
internal/mcp/           protocol version and _meta checks
internal/router/        discover, merged tools/list, tools/call routing
internal/httpfront/     Streamable HTTP endpoint and header checks
internal/progress/      progress token rewriting and routing
internal/auth/          API key identification
internal/policy/        rules, argument checks, and the guard in front of the router
internal/ratelimit/     per-agent, per-tool token buckets
internal/audit/         redaction, audit queue, SQLite store, and the audit layer
internal/metrics/       Prometheus metrics, fed by the audit layer
internal/tracing/       OpenTelemetry spans and trace context for upstream calls
internal/pinning/       tool description hashing and quarantine (planned)
testdata/fakeupstream/  entry point for the fake server binary
```

The fake server's logic lives in `internal/fakeupstream` so that `go test ./...` and `go vet ./...` check it. Go's `./...` pattern skips `testdata` folders, so `testdata/fakeupstream` only holds a small `main.go` that wires the server to stdin and stdout.

## Requirements

- Go 1.26 or newer

Dependencies are tracked in `go.mod`. There are four:

- [`go.yaml.in/yaml/v4`](https://pkg.go.dev/go.yaml.in/yaml/v4) for the config and policy files, since Go's standard library has no YAML parser. It is the YAML organization's maintained version of go-yaml. The older `gopkg.in/yaml.v3` was marked unmaintained in 2025.
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) for the audit log. It is SQLite translated to Go, so builds need no C compiler. It is slower than the C version, which doesn't matter for a log written in the background. It brings in its own set of `modernc.org` packages.
- [`prometheus/client_golang`](https://pkg.go.dev/github.com/prometheus/client_golang) for `/metrics`. Writing the Prometheus text format by hand is possible, but histograms and the Go runtime metrics are real work to get right.
- [OpenTelemetry Go](https://pkg.go.dev/go.opentelemetry.io/otel) (`otel`, `otel/sdk`, and the OTLP/HTTP trace exporter) for tracing. The exporter shares generated code with the gRPC exporter, so it pulls in `google.golang.org/grpc` and makes the binary about 7 MB bigger (20.8 MB to 28.0 MB, measured), even though the gateway never opens a gRPC connection. We kept it because it is maintained and handles retries, TLS, and every `OTEL_EXPORTER_OTLP_*` setting.

## Build and test

```sh
make check    # gofmt check, go vet, and go test ./... -race
make test     # tests only
make fmt      # format all files
make build    # builds bin/gateway
```

## Run

```sh
gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db [-call-timeout 60s] [-log-level info]
gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db -http 127.0.0.1:8080 [-allow-origin http://localhost:3000]...
gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db -http 0.0.0.0:8443 -tls-cert cert.pem -tls-key key.pem
gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db -http :8080 -behind-tls-proxy
gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db -metrics 127.0.0.1:9090 [-metrics-allow-remote]
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db -trace
gateway keygen -agent NAME
gateway audit -db audit.db [-agent NAME] [-tool NAME] [-decision D] [-since 1h] [-limit N] [-json]
```

Without `-http`, the gateway speaks MCP over stdin and stdout. With it, the gateway serves `POST /mcp` and doesn't read stdin. On a loopback address (`127.0.0.1`, `::1`, `localhost`) plain HTTP is fine. Any other address needs encryption, because every request carries an API key: either `-tls-cert` and `-tls-key` (HTTPS, TLS 1.2 or newer), or `-behind-tls-proxy` when nginx, Caddy, or a load balancer in front handles HTTPS. Without one of those the gateway refuses to start. Browsers send an `Origin` header, and only origins listed with `-allow-origin` are accepted; requests without `Origin` (curl, SDKs, agents) are fine.

The config file lists the upstream servers. Each name becomes the prefix of that server's tools, so it must be unique and use only lowercase letters, digits, and `-`:

```yaml
upstreams:
  - name: fs                      # tools show up as fs__<tool>
    command: /usr/local/bin/fs-server
    args: ["--root", "/tmp/agent-workspace"]

agents:
  - name: ci-bot                  # who is calling; the policy refers to this name
    key_sha256: <SHA-256 of the agent's API key, 64 lowercase hex characters>
```

At least one agent is required. The file holds only the SHA-256 of each API key, never the key itself; a config with a raw `mcpgw_...` key in it is rejected.

### API keys

`gateway keygen -agent ci-bot` makes a new key. The key goes to stdout and is shown only once; the lines to paste under `agents:` (name and hash) go to stderr. Then the agent sends the key:

- **stdio:** in the `MCP_GATEWAY_API_KEY` environment variable, as the MCP spec suggests for stdio. It is checked once, before any upstream starts; without a valid key the gateway exits with code 1. A Claude Desktop entry looks like this:

  ```json
  {"mcpServers": {"gateway": {
    "command": "/path/to/bin/gateway",
    "args": ["-config", "/path/to/gateway.yaml", "-policy", "/path/to/policy.yaml", "-audit-db", "/path/to/audit.db"],
    "env": {"MCP_GATEWAY_API_KEY": "mcpgw_..."}
  }}}
  ```

- **HTTP:** in `Authorization: Bearer <key>` on every request. A missing or wrong key gets `401` with `WWW-Authenticate: Bearer`, and the same message either way, so the answer doesn't reveal which keys exist. Keys in the URL are never read.

The key stops at the gateway. Upstreams never see it, and logs name the agent, never the key.

### Policy

`-policy` is required. The policy file says which agent may call which tool, and everything it doesn't allow is denied:

```yaml
roles:
  reader:
    allow:
      - tool: github__*            # every tool of one upstream
      - tool: fs__read_file        # one tool, any arguments
  writer:
    allow:
      - tool: fs__write_file
        args:                      # only these arguments may be sent
          path:    { path_prefix: [/tmp/agent-workspace] }
          content: { max_length: 100000 }
          mode:    {}              # allowed with any value, or left out
      - tool: db__query
        args:
          sql: { deny_patterns: ['\bdrop\b', '\btruncate\b'] }

agents:
  ci-bot: [reader]
  analyst: [reader, writer]

limits:
  default: { rate: 60/minute, burst: 10 }   # every tool without its own entry
  tools:
    - tool: db__query
      rate: 5/minute
      burst: 2
    - tool: github__*
      rate: 100/minute
      burst: 20
```

- An agent sees only the tools it may call in `tools/list`. A call to any other tool gets the same `Unknown tool` error as a tool that doesn't exist, so agents can't probe for hidden tools.
- `*` only works as the whole tool part (`github__*`). Patterns like `fs__read_*` are rejected, because they would also match tools an upstream adds later.
- An argument with checks must be sent, because the upstream's default for a missing argument could be anything. A rule with `args:` denies arguments it doesn't list.
- `path_prefix` paths must be absolute. They are cleaned before the check, so `/tmp/agent-workspace/../../etc/passwd` counts as `/etc/passwd`. Whole folder names are compared, so `/tmp/agent-workspace-evil` doesn't match. Null bytes and backslashes are denied.
- `deny_patterns` are Go regular expressions, always matched without regard to case. `max_length` counts bytes.
- Arguments with a repeated key or invalid UTF-8 are denied, so the gateway can't check one value while the upstream reads another.
- When arguments fail a check, the agent gets a tool result with `isError: true`, like `Denied by gateway policy: argument "path" is outside the allowed folders`. The spec says clients should show these to the model, so it can try again. The message never includes the folders, the patterns, or the argument's value. The log line names the agent, the tool, the role, and which pattern matched.
- `limits:` is required and needs a `default`, so no tool is ever unlimited. Each agent gets its own token bucket for each tool: `burst` calls can come at once, and `rate` says how fast they come back. An exact tool entry wins over `upstream__*`, which wins over `default`. With `github__*`, each GitHub tool is counted on its own. A call over the limit gets JSON-RPC error `-31029` with `data.retryAfterMs`, the wait until the next token. Over HTTP the status is `429` with a `Retry-After` header in seconds. The code sits outside the range the spec reserves (`-32768` to `-32000`), as the spec asks. Calls the policy denies don't use up tokens, and only `tools/call` is counted.
- If the policy file is missing, invalid, or names an upstream or agent the config doesn't have, the gateway refuses to start. If the guard can't check something, the call is denied.

Limits to know about:

- Paths are checked as text. The gateway can't follow symlinks, because the files are on the upstream's side. `%2e%2e` isn't decoded, since the filesystem server won't decode it either.
- A SQL blocklist is easy to get around: `DR/**/OP TABLE` splits the keyword with a comment and passes `\bdrop\b`. Use it as a guard rail only. The real protection is a database user that can only read.

### Audit log

`-audit-db` is required. Every `tools/call` from an authenticated agent gets one row in a SQLite file, whatever happened to it: allowed, denied by the policy, rate limited, or failed upstream. A row has:

- a request ID, the time (UTC), the agent, and the tool
- the arguments, with secrets replaced by `[REDACTED]`
- the decision (`allowed`, `denied`, `rate_limited`), the reason, and the rule or limit involved
- how it ended (`ok`, `tool_error`, `rpc_error` with its code, or `no_response`) and the time spent upstream

What the tool returned is never stored.

Secrets are found two ways. Values under names like `password`, `token`, `apiKey`, or `authorization` are hidden whole. Strings shaped like known secrets (this gateway's keys, `Bearer` values, `sk-` keys, GitHub, AWS, and Slack tokens, private keys, and card numbers that pass the Luhn check) are hidden wherever they appear. A plain password under an innocent name like `note` can't be recognized and is stored as is.

The table is append-only: SQLite triggers reject every `UPDATE` and `DELETE`.

A call reserves room in the audit queue before it is checked or forwarded. If the database falls behind or fails, the queue fills, and after `-audit-wait` (default 100ms) new calls get `-32603 "audit log unavailable"` without running. A call never runs without a record. Failed writes are retried until they succeed. `-audit-queue` (default 1024) also caps how many tool calls can be in flight at once. On shutdown the gateway waits up to 10 seconds for the queue to be written.

To read it, even while the gateway runs:

```sh
gateway audit -db bin/audit.db                                  # newest 50 rows, oldest first
gateway audit -db bin/audit.db -agent ci-bot -since 1h
gateway audit -db bin/audit.db -tool 'github__*' -decision denied
gateway audit -db bin/audit.db -limit 1000 -json | jq .          # one JSON object per line
```

`-since` takes a duration (`30m`, `2h`) or a time (`2026-11-14T10:00:00Z`). `gateway audit` opens the file read-only and never creates it. In the table, control characters in agent-supplied text (tool names, reasons, arguments) are shown escaped, like `\u001b`, so a crafted tool name can't send escape codes to your terminal. Long arguments are cut short in the table; `-json` shows them whole.

Commands run directly, not through a shell. Only the command is logged, since arguments can hold tokens. Exit codes: 0 for a normal stop, 1 if the config, policy, or audit log path is bad or an upstream fails or exits, 2 for a bad command line.

To try it with the fake server and [`testdata/config/example-policy.yaml`](testdata/config/example-policy.yaml), which lets the agent `example` call `fake__echo`, `fake__add`, and `fake__count`:

```sh
make build
go build -o bin/fakeupstream ./testdata/fakeupstream
printf 'upstreams:\n  - name: fake\n    command: ./bin/fakeupstream\nagents:\n' > bin/my.yaml
# The key goes into the variable; the agent lines are appended to the config.
export MCP_GATEWAY_API_KEY="$(./bin/gateway keygen -agent example 2>>bin/my.yaml)"
echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__add","arguments":{"a":2,"b":3},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' | ./bin/gateway -config bin/my.yaml -policy testdata/config/example-policy.yaml -audit-db bin/audit.db
```

The reply on stdout includes `"structuredContent":{"sum":5}`. Log lines go to stderr. Change the tool to `fake__fail` and the reply is `Unknown tool: fake__fail`, with a `call denied` line in the log. `make run` starts the gateway with a fresh key and a policy that allows every fake tool, and lets you type requests in.

Over HTTP:

```sh
./bin/gateway -config bin/my.yaml -policy testdata/config/example-policy.yaml -audit-db bin/audit.db -http 127.0.0.1:8080 &
curl -s http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $MCP_GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: tools/call' -H 'Mcp-Name: fake__count' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__count","arguments":{"n":2},"_meta":{"progressToken":"t1","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
```

Because the request has a `progressToken`, the reply is an SSE stream: two `notifications/progress` events with the token `t1`, then the result. Without a token, or if no progress arrives, the reply is plain JSON.

### Metrics

`-metrics ADDR` serves Prometheus metrics at `GET /metrics` on its own address, in both stdio and HTTP mode. The endpoint has no API key check and shows agent and tool names, so the gateway only lets it listen on loopback unless you add `-metrics-allow-remote`. If you do, make sure only your Prometheus server can reach it.

| Metric | Labels | What it shows |
|---|---|---|
| `mcpgw_tool_calls_total` | agent, tool, decision, status | every tool call and how it ended |
| `mcpgw_tool_call_denials_total` | agent, tool, reason | refused calls; reason is `tool`, `arguments`, or `rate_limited` |
| `mcpgw_tool_call_duration_seconds` | decision | total time a call spent in the gateway |
| `mcpgw_upstream_latency_seconds` | tool | time forwarded calls spent upstream |
| `mcpgw_audit_refused_total` | | calls refused because the audit queue was full |
| `mcpgw_audit_queue_slots_used`, `mcpgw_audit_queue_slots` | | how full the audit queue is |

Go runtime and process metrics (`go_*`, `process_*`) are included too.

The counts come from the audit layer, so metrics and the audit log always count the same calls. Comparing the two time histograms shows how much time the gateway itself adds.

Prometheus keeps one series per distinct label value, so every label comes from a short known list. Agent names come from the config, and anything else shows as `none`. Tool names are the ones an upstream listed, and anything else shows as `other`, so an agent making up tool names can't create new series. One side effect: a real tool also shows as `other` until the gateway has fetched its upstream's tool list. That happens on `tools/list`, and over HTTP also on the first call to one of that upstream's tools, since checking the `Mcp-Param` headers needs the schema.

```sh
./bin/gateway -config bin/my.yaml -policy testdata/config/example-policy.yaml -audit-db bin/audit.db -http 127.0.0.1:8080 -metrics 127.0.0.1:9090 &
curl -sS http://127.0.0.1:9090/metrics | grep '^mcpgw_'
```

### Tracing

`-trace` sends OpenTelemetry traces over OTLP/HTTP. It is off by default. Everything else comes from the standard OpenTelemetry environment variables:

- `OTEL_EXPORTER_OTLP_ENDPOINT`: where to send them. The default is `https://localhost:4318`; a local collector without TLS needs `http://localhost:4318`.
- `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`: how many requests to keep. The default keeps all of them.
- `OTEL_SERVICE_NAME`: the name in the trace viewer. The default is `mcp-gateway`.
- `OTEL_EXPORTER_OTLP_HEADERS`: headers for a collector that needs a token.

Each request gets a span named like `tools/call fs__read_file`, and each call the gateway makes to an upstream gets a span inside it, named like `tools/call read_file`. Spans carry the agent, tool, policy decision and reason, and `error.type`. They never carry arguments. Names and attributes follow the [OpenTelemetry conventions for MCP](https://github.com/open-telemetry/semantic-conventions-genai). The gateway puts its own `traceparent` in the `_meta` it sends upstream, so an upstream that traces too shows up under the gateway's span.

A client can send its own trace context in `_meta`, but that is untrusted input: it could pick trace IDs or force every request to be recorded. So each request starts a new trace, with a link back to the client's trace. The client's `traceparent`, `tracestate`, and `baggage` are removed before a call goes upstream, with or without `-trace`.

If the collector is down, tool calls work as usual. Failed sends are logged as warnings, and shutdown waits at most 5 seconds to send the last spans.

To look at traces locally, run [Jaeger](https://www.jaegertracing.io/) in Docker. It takes OTLP on port 4318 and shows traces at http://localhost:16686:

```sh
docker run --rm -d --name jaeger -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:latest
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 bin/gateway -config gateway.yaml -policy policy.yaml -audit-db audit.db -trace
```

Pick `mcp-gateway` under Service and click Find Traces. Jaeger keeps traces in memory, so they are gone once you run `docker stop jaeger`. Two things to know:

- The search only covers the Lookback window, "Last 1 hour" by default. Older traces are still there; make the window bigger to see them.
- A span is named after its tool only once the gateway has fetched that upstream's tool list. Calls sent before that are named plain `tools/call`. The tool name is still in the `gen_ai.tool.name` attribute.

## Benchmarks

Measured with `make bench` on a MacBook Air (Apple M3, 8 cores, macOS), Go 1.26.4, at commit `61cd5be` with no uncommitted changes. Every result, down to each single call's time, is saved in [bench/results](bench/results), and the numbers below come from those two files.

Each run has 5 rounds. The tables show the median round, with the lowest and highest round in brackets.

### What is measured

Every call is `tools/call` to the fake upstream's `echo` tool with a short path as the text. The fake upstream does almost no work, so what's left is the time the gateway itself adds: parsing, auth, policy, rate limit, audit, routing, and the extra pipe hop.

- **Added latency (stdio).** 5,000 calls one at a time straight to the fake upstream, then 5,000 through the gateway, after 500 warm-up calls each. Added latency is the gateway's p50 (or p99) minus the direct one. The order flips each round so neither side always gets the warmer machine.
- **Throughput (HTTP).** The gateway runs with `-http` on loopback, and 1, 8, or 32 clients send calls back to back for 5 seconds after a 1 second warm-up. Calls/s counts answered calls only.
- **Two policies.** `minimal` allows every tool with no checks. `realistic` has two roles, the matching rule comes after three others, and the `text` argument is checked against a length limit, two allowed folders, and four deny patterns. Both have rate limits too high to reach, so the limiter does its work but never says no.

### Results

Added by the gateway over stdio, one call at a time (direct calls take about 22µs at p50):

| Policy | p50 | p99 |
|---|---|---|
| minimal | 47.6µs [47.2 to 49.0] | 154.2µs [131.7 to 155.3] |
| realistic | 53.2µs [51.9 to 65.2] | 159.6µs [153.5 to 225.7] |

HTTP throughput:

| Clients | minimal calls/s | realistic calls/s | p99 (minimal / realistic) |
|---|---|---|---|
| 1 | 10,412 [10,373 to 10,445] | 9,676 [9,530 to 9,879] | 0.24ms / 0.26ms |
| 8 | 19,290 [19,198 to 19,357] | 17,924 [17,298 to 17,947] | 5.4ms / 5.4ms |
| 32 | 20,063 [19,592 to 20,305] | 18,460 [17,357 to 19,140] | 13.9ms / 14.4ms |

No call was refused in either run (about 2.4 million HTTP calls in all).

### What the numbers say

- The gateway adds under 0.1ms at p50 and under 0.25ms at p99 to a stdio call, in every round of both runs.
- The realistic policy costs about 6µs per call and about 7% of throughput at 8 clients. The lowest-to-highest ranges of the two runs don't overlap, so the difference is bigger than the noise between rounds.
- Throughput stops growing at about 8 clients. Past that, more clients only make each call wait longer (p99 goes from 5ms to 14ms).
- The limit is the audit log. Measured on its own with `go test ./internal/audit -bench RecorderSQLite`, it saves about 30,000 to 37,000 entries/s on this laptop, while one upstream pipe handles about 120,000 calls/s (`go test ./internal/upstream -bench ConnCallsInParallel`). These two came from profiling at commit `7fcc2d6` and are not in the results files. Bigger batches and `synchronous=NORMAL` didn't help.
- When calls come in faster than the log can save them, the audit queue fills and the gateway refuses calls with "audit log unavailable" instead of running them unrecorded. In earlier runs with 128 clients this happened, and rarely (3 calls in 250,000) it also happened with 1 client, which points to the writer sometimes pausing for about 100ms.

### Caveats

- One laptop, one run of each policy. Results move by a few percent between runs, and core placement on the M3 matters (running at background priority made everything 3 to 5 times slower).
- Load clients, gateway, and upstream share the same 8 cores, so the gateway gets less CPU than it would on its own machine.
- Everything is on loopback with a fake upstream. A real upstream doing real work will make the gateway's share of each call much smaller.

To run it yourself, close busy programs and run `make bench`. It takes several minutes and writes two new files into `bench/results`. `bin/loadtest -h` lists the options.

## Known limits

- If an upstream stops reading its stdin, a write to it can block the caller.
- Killing an upstream does not kill processes it started itself (no process groups yet).
- Upstreams inherit the gateway's environment variables.
- If an upstream exits while running, the gateway exits too. There is no reconnect yet.
- Upstreams are started one after another, so startup time adds up with many upstreams.
- A slow HTTP or stdio client can hold up an upstream's notifications, since they are written on that upstream's read loop.
- The gateway doesn't support `subscriptions/listen` yet, so list-changed notifications don't reach clients.

These are planned for Week 5.

## Roadmap

1. Passthrough proxy over stdio, standard library only (done)
2. Multiple upstreams, tool namespacing, Streamable HTTP frontend (done)
3. API key auth and the policy engine (done)
4. Rate limiting, audit log, metrics
5. Timeouts, circuit breakers, description pinning, load tests, demo

Benchmark numbers are in [Benchmarks](#benchmarks).
