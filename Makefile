.PHONY: all build test vet fmt fmt-check check run bench clean

BIN_DIR := bin
GATEWAY := $(BIN_DIR)/gateway

all: check build

build:
	go build -o $(GATEWAY) ./cmd/gateway

test:
	go test ./... -race

# Go skips testdata folders in ./... patterns (even ./testdata/...), so the
# fake servers there are listed by exact path.
vet:
	go vet ./... ./testdata/fakeupstream

fmt:
	gofmt -w .

# Fails if any file is not gofmt-formatted. Used by check so CI-style runs catch it.
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

# Everything that must pass before a change is done.
check: fmt-check vet test

# Runs the gateway over stdio in front of the fake upstream, with a fresh key
# for a "dev" agent that may call every fake tool. Type JSON-RPC lines into
# it. keygen prints the key on stdout (captured straight into the
# environment) and the config lines with its hash on stderr (appended to
# bin/run.yaml), so the key never hits disk.
run: build
	go build -o $(BIN_DIR)/fakeupstream ./testdata/fakeupstream
	@printf 'upstreams:\n  - name: fake\n    command: ./bin/fakeupstream\nagents:\n' > $(BIN_DIR)/run.yaml
	@printf 'roles:\n  all:\n    allow:\n      - tool: fake__*\nagents:\n  dev: [all]\nlimits:\n  default: { rate: 60/minute, burst: 10 }\n' > $(BIN_DIR)/run-policy.yaml
	@MCP_GATEWAY_API_KEY="$$($(GATEWAY) keygen -agent dev 2>>$(BIN_DIR)/run.yaml)" $(GATEWAY) -config $(BIN_DIR)/run.yaml -policy $(BIN_DIR)/run-policy.yaml -audit-db $(BIN_DIR)/audit.db

# Measures the gateway (see cmd/loadtest) and saves every result as JSON in
# bench/results, one file per policy (minimal, realistic), named by the UTC
# time it started. Takes several minutes; close other busy programs first.
# go build stamps the git commit into loadtest, so the results say which
# code was measured.
bench: build
	go build -o $(BIN_DIR)/fakeupstream ./testdata/fakeupstream
	go build -o $(BIN_DIR)/loadtest ./cmd/loadtest
	@mkdir -p bench/results
	@stamp=$$(date -u +%Y%m%dT%H%M%SZ); \
	for policy in minimal realistic; do \
		echo "== policy $$policy"; \
		$(BIN_DIR)/loadtest -policy $$policy -out bench/results/$$stamp-$$policy.json || exit 1; \
	done

clean:
	rm -rf $(BIN_DIR)
