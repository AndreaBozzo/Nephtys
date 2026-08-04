# Makefile for Nephtys

# Load .env file if it exists
-include .env
export

BINARY := nephtys
CMD := ./cmd/nephtys

# Stamped into the image by docker-build. Override for a release:
#   make docker-build VERSION=v0.3.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help build run test bench coverage fmt vet lint clean check-examples docker-up docker-up-full docker-down docker-build all

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary
	go build -o $(BINARY) $(CMD)

run: ## Run the application, exporting .env first if it exists
	@set -a; [ -f .env ] && . ./.env; set +a; go run $(CMD)

test: ## Run all tests
	go test -race -cover ./...

bench: ## Run benchmarks (pipeline publish path and broker)
	go test -run XXX -bench . -benchmem ./internal/pipeline/ ./internal/broker/

coverage: ## Generate HTML coverage report
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

fmt: ## Format code
	gofmt -s -w .

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint (requires golangci-lint installed locally)
	golangci-lint run

check-examples: ## Validate every docs/examples/*.json with --config-check
	@set -e; \
	count=0; \
	for f in docs/examples/*.json; do \
		[ -e "$$f" ] || continue; \
		go run $(CMD) --config-check "$$f"; \
		count=$$((count + 1)); \
	done; \
	if [ "$$count" -eq 0 ]; then \
		echo "no example configs found in docs/examples/ — did the directory move?" >&2; \
		exit 1; \
	fi; \
	echo "$$count example config(s) valid"

clean: ## Remove build artifacts
	rm -f $(BINARY)
	go clean

docker-up: ## Start NATS, Prometheus and a provisioned Grafana (run Nephtys on the host)
	docker compose up -d

docker-up-full: ## Same, plus Nephtys itself from the published GHCR image
	docker compose --profile nephtys up -d

docker-down: ## Stop the compose stack
	docker compose --profile nephtys down

docker-build: ## Build the production Docker image (override VERSION=v0.3.0)
	docker build --build-arg VERSION="$(VERSION)" -t $(BINARY):latest .

all: fmt vet test ## Run fmt, vet, and tests
