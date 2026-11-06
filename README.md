# MCP Gateway in Go

A gateway that sits between AI agents and MCP servers.

Agents connect to the gateway as if it were a single MCP server. Behind it, the gateway connects to many upstream MCP servers (filesystem, database, GitHub, and so on). Every tool call goes through the gateway, so it can decide who may call which tool, how often, and with which arguments. It logs every call.

> Status: the gateway runs over stdio in front of several upstream MCP servers, merges their tools under prefixed names, and routes each tool call to the right server. Policy, auth, rate limits, and audit are not built yet, so right now every call is allowed.

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
                                                   ┌──▶ upstream "fs"     (child process)
client ──stdin/stdout──▶ proxy ──▶ router ─────────┤
                                                   └──▶ upstream "github" (child process)
```

- **`cmd/gateway`** is the program. It reads the config file, starts every upstream, and runs the proxy on its own stdin and stdout. Logs go to stderr. It stops when the client closes stdin, on Ctrl+C or SIGTERM, or when any upstream exits.
- **`internal/config`** loads and checks the YAML config.

- **`internal/jsonrpc`** parses and validates every message as untrusted input, and reads and writes one JSON message per line, with a size limit.
- **`internal/upstream`** starts each MCP server as a child process and checks it with `server/discover` before use. If any upstream fails to start, the gateway refuses to start. It gives each request its own ID and matches each response to the caller waiting for it. On shutdown it closes each child's stdin, then sends SIGTERM, then SIGKILL.
- **`internal/proxy`** runs the client session. Each request runs on its own goroutine with a timeout, and the reply gets the client's original ID back. When a client cancels a request, the matching upstream call is cancelled.
- **`internal/router`** decides what each request does. It answers `server/discover` itself and checks the protocol version on every request. `tools/list` asks every upstream at once and returns one list with names like `fs__read_file`. If one upstream fails, its tools are left out and the rest are still listed. `tools/call` goes to the upstream named by the prefix, with the prefix removed. Other methods get "method not found" for now.
- **`internal/fakeupstream`** is a small MCP server with `echo`, `add`, and `fail` tools, used in tests.

When an upstream doesn't reply, the client gets a JSON-RPC error (`-32603`) saying "upstream timed out", "upstream unavailable", or "upstream error". The details go to the log only. Calls are never retried.

## Planned features

| Feature | What it does | Status |
|---|---|---|
| Proxy | Runs the MCP session over JSON-RPC 2.0: timeouts, cancellation, request IDs | Built |
| Multiple upstreams | Merges tools from several servers and prefixes names (`fs__read_file`) so they don't collide | Built |
| Transports | stdio (for local clients like Claude Desktop) and streamable HTTP | stdio built |
| Auth | API keys per agent, stored hashed | Planned |
| Policy engine | YAML rules, default deny, argument checks like path prefixes and blocked SQL | Planned |
| Rate limiting | Token bucket per agent and per tool | Planned |
| Audit log | Append-only JSON in SQLite with redacted arguments | Planned |
| Observability | Prometheus metrics and OpenTelemetry traces | Planned |
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
internal/auth/          API key identification (planned)
internal/policy/        rules engine, YAML loading (planned)
internal/ratelimit/     per-agent, per-tool limits (planned)
internal/audit/         append-only structured log (planned)
internal/metrics/       Prometheus and OpenTelemetry (planned)
internal/pinning/       tool description hashing and quarantine (planned)
testdata/fakeupstream/  entry point for the fake server binary
```

The fake server's logic lives in `internal/fakeupstream` so that `go test ./...` and `go vet ./...` check it. Go's `./...` pattern skips `testdata` folders, so `testdata/fakeupstream` only holds a small `main.go` that wires the server to stdin and stdout.

## Requirements

- Go 1.26 or newer

Dependencies are tracked in `go.mod`. The only one so far is [`go.yaml.in/yaml/v4`](https://pkg.go.dev/go.yaml.in/yaml/v4) for the config file, since Go's standard library has no YAML parser. It is the YAML organization's maintained version of go-yaml. The older `gopkg.in/yaml.v3` was marked unmaintained in 2025.

## Build and test

```sh
make check    # gofmt check, go vet, and go test ./... -race
make test     # tests only
make fmt      # format all files
make build    # builds bin/gateway
```

## Run

```sh
gateway -config gateway.yaml [-call-timeout 60s] [-log-level info]
```

The config file lists the upstream servers. Each name becomes the prefix of that server's tools, so it must be unique and use only lowercase letters, digits, and `-`:

```yaml
upstreams:
  - name: fs                      # tools show up as fs__<tool>
    command: /usr/local/bin/fs-server
    args: ["--root", "/tmp/agent-workspace"]
```

Commands run directly, not through a shell. Only the command is logged, since arguments can hold tokens. Exit codes: 0 for a normal stop, 1 if the config is bad or an upstream fails or exits, 2 for a bad command line.

To try it with the fake server ([`testdata/config/example.yaml`](testdata/config/example.yaml) names it `fake`):

```sh
make build
go build -o bin/fakeupstream ./testdata/fakeupstream
echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fake__add","arguments":{"a":2,"b":3},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' | ./bin/gateway -config testdata/config/example.yaml
```

The reply on stdout includes `"structuredContent":{"sum":5}`. Log lines go to stderr. `make run` starts the same setup and lets you type requests in.

## Known limits

- If an upstream stops reading its stdin, a write to it can block the caller.
- Killing an upstream does not kill processes it started itself (no process groups yet).
- Upstreams inherit the gateway's environment variables.
- If an upstream exits while running, the gateway exits too. There is no reconnect yet.
- Upstreams are started one after another, so startup time adds up with many upstreams.

These are planned for Week 5.

## Roadmap

1. Passthrough proxy over stdio, standard library only (done)
2. Multiple upstreams, tool namespacing, HTTP frontend, sessions
3. API key auth and the policy engine
4. Rate limiting, audit log, metrics
5. Timeouts, circuit breakers, description pinning, load tests, demo

Benchmark numbers will be added here only after we measure them ourselves.
