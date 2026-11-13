# Common tasks. Run `make` (or `make help`) to list them.

BIN     := bin/asana-extractor
COVER   := cover.out
ARGS    ?=

.DEFAULT_GOAL := help
.PHONY: help run once build test cover lint fmt check clean

help: ## List the available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

run: ## Poll every 5m until Ctrl+C (extra flags: make run ARGS="-interval 30s")
	go run . $(ARGS)

once: ## Extract once and exit
	go run . -once $(ARGS)

build: ## Build the binary into bin/
	go build -o $(BIN) .

test: ## Run all tests with the race detector
	go test -race ./...

cover: ## Test coverage across packages (total + per function in cover.out)
	go test -coverpkg=./... -coverprofile=$(COVER) ./...
	go tool cover -func=$(COVER) | tail -1

lint: ## Check formatting and vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
	go vet ./...

fmt: ## Format the code
	gofmt -w .

check: lint test ## Everything CI would run: lint + tests

clean: ## Remove build and coverage output
	rm -rf bin $(COVER)
