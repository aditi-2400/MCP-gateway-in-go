.PHONY: all build test vet fmt fmt-check check run clean

BIN_DIR := bin
GATEWAY := $(BIN_DIR)/gateway

all: check build

build:
	go build -o $(GATEWAY) ./cmd/gateway

test:
	go test ./... -race

vet:
	go vet ./...

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

run:
	go run ./cmd/gateway

clean:
	rm -rf $(BIN_DIR)
