# MCP Gateway in Go

A gateway that sits between AI agents and MCP servers.

Agents connect to the gateway as if it were a single MCP server. Behind it, the gateway connects to many upstream MCP servers (filesystem, database, GitHub, and so on). Every tool call goes through the gateway, so it can decide who may call which tool, how often, and with which arguments. It logs every call.

> Status: early. Nothing is built yet. See the roadmap below.

## Why

MCP gives agents direct access to real tools. Once an agent can write files or run SQL, you need one place that answers:

- Which agent made this call?
- Is it allowed to call this tool with these arguments?
- Is it calling too often?
- What happened, and can we look it up later?

The gateway is that place.

## Planned features

| Feature | What it does |
|---|---|
| Passthrough proxy | Forwards `initialize`, `tools/list`, and `tools/call` over JSON-RPC 2.0 |
| Multiple upstreams | Merges tools from several servers and prefixes names (`fs__read_file`) so they don't collide |
| Transports | stdio (for local clients like Claude Desktop) and streamable HTTP |
| Auth | API keys per agent, stored hashed |
| Policy engine | YAML rules, default deny, argument checks like path prefixes and blocked SQL |
| Rate limiting | Token bucket per agent and per tool |
| Audit log | Append-only JSON in SQLite with redacted arguments |
| Observability | Prometheus metrics and OpenTelemetry traces |
| Resilience | Timeouts, circuit breakers, no automatic retries on tools with side effects |
| Description pinning | Hashes tool descriptions and quarantines a tool if its description changes |

## Design rules

- Default deny. If no rule allows a call, it is blocked.
- Fail closed. If the policy engine errors, the call is denied.
- No automatic retries on tool calls that could have side effects.
- Secrets in arguments are redacted before logging.
- All incoming JSON is treated as untrusted.

## Planned layout

```
cmd/gateway/          entry point
internal/jsonrpc/     message types, framing, errors
internal/upstream/    spawn and talk to upstream servers
internal/proxy/       request routing and passthrough handlers
internal/auth/        API key identification
internal/policy/      rules engine, YAML loading
internal/ratelimit/   per-agent, per-tool limits
internal/audit/       append-only structured log
internal/metrics/     Prometheus and OpenTelemetry
internal/pinning/     tool description hashing and quarantine
testdata/             fake upstream servers and sample configs
```

## Requirements

- Go 1.26 or newer

Dependencies are tracked in `go.mod`. Week 1 uses only the standard library.

## Build and test

These commands will work once the code is in place:

```sh
go build ./...
go vet ./...
go test ./... -race
```

## Roadmap

1. Passthrough proxy over stdio, standard library only
2. Multiple upstreams, tool namespacing, HTTP frontend, sessions
3. API key auth and the policy engine
4. Rate limiting, audit log, metrics
5. Timeouts, circuit breakers, description pinning, load tests, demo

Benchmark numbers will be added here only after we measure them ourselves.
