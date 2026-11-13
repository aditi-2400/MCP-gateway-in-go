.PHONY: all build test vet fmt fmt-check check run clean

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
# for a "dev" agent. Type JSON-RPC lines into it. keygen prints the key on
# stdout (captured straight into the environment) and the config lines with
# its hash on stderr (appended to bin/run.yaml), so the key never hits disk.
run: build
	go build -o $(BIN_DIR)/fakeupstream ./testdata/fakeupstream
	@printf 'upstreams:\n  - name: fake\n    command: ./bin/fakeupstream\nagents:\n' > $(BIN_DIR)/run.yaml
	@MCP_GATEWAY_API_KEY="$$($(GATEWAY) keygen -agent dev 2>>$(BIN_DIR)/run.yaml)" $(GATEWAY) -config $(BIN_DIR)/run.yaml

clean:
	rm -rf $(BIN_DIR)
