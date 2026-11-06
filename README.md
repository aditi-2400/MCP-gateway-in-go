# MCP Gateway in Go

A gateway that sits between AI agents and MCP servers.

Agents connect to the gateway as if it were a single MCP server. Behind it, the gateway connects to many upstream MCP servers (filesystem, database, GitHub, and so on). Every tool call goes through the gateway, so it can decide who may call which tool, how often, and with which arguments. It logs every call.

> Status: Week 1 done. The gateway runs as a passthrough proxy over stdio in front of one upstream MCP server. Policy, auth, rate limits, and audit come in later weeks, so right now it forwards everything.

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
client ──stdin/stdout──▶ gateway (proxy ──▶ upstream.Conn) ──stdin/stdout──▶ MCP server (child process)
```

- **`cmd/gateway`** is the program. It starts the upstream server, runs the proxy on its own stdin and stdout, and logs to stderr. It stops when the client closes stdin, on Ctrl+C or SIGTERM, or when the upstream exits.

- **`internal/jsonrpc`** parses and validates every message as untrusted input, and reads and writes one JSON message per line, with a size limit.
- **`internal/upstream`** starts the MCP server as a child process. It gives each request its own ID and matches each response to the caller waiting for it. On shutdown it closes the child's stdin, then sends SIGTERM, then SIGKILL.
- **`internal/proxy`** forwards each client request on its own goroutine with a timeout, then puts the client's original ID back on the reply. When a client cancels a request, the matching upstream call is cancelled. Notifications are forwarded both ways.
- **`internal/fakeupstream`** is a small MCP server with `echo`, `add`, and `fail` tools, used in tests.

When the upstream doesn't reply, the client gets a JSON-RPC error (`-32603`) saying "upstream timed out", "upstream unavailable", or "upstream error". The details go to the log only. Calls are never retried.

## Planned features

| Feature | What it does | Status |
|---|---|---|
| Passthrough proxy | Forwards `server/discover`, `tools/list`, `tools/call`, and other methods over JSON-RPC 2.0 | Built |
| Multiple upstreams | Merges tools from several servers and prefixes names (`fs__read_file`) so they don't collide | Planned |
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
gateway [-call-timeout 60s] [-log-level info] -- upstream-command [args...]
```

Everything after `--` is the command that starts the upstream MCP server. Only its path is logged, since arguments can hold tokens. Exit codes: 0 for a normal stop, 1 if the upstream exits or fails, 2 for a bad command line.

To try it with the fake server:

```sh
make build
go build -o bin/fakeupstream ./testdata/fakeupstream
echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' | ./bin/gateway -- ./bin/fakeupstream
```

The reply on stdout includes `"structuredContent":{"sum":5}`. Log lines go to stderr. `make run` starts the same setup and lets you type requests in.

## Known limits

- If an upstream stops reading its stdin, a write to it can block the caller.
- Killing an upstream does not kill processes it started itself (no process groups yet).
- Upstreams inherit the gateway's environment variables.

These are planned for Week 5.

## Roadmap

1. Passthrough proxy over stdio, standard library only (done)
2. Multiple upstreams, tool namespacing, HTTP frontend, sessions
3. API key auth and the policy engine
4. Rate limiting, audit log, metrics
5. Timeouts, circuit breakers, description pinning, load tests, demo

Benchmark numbers will be added here only after we measure them ourselves.
