# Makefile for Nephtys

# Load .env file if it exists
-include .env
export

BINARY := nephtys
CMD := ./cmd/nephtys

.PHONY: help build run test fmt vet lint clean docker-up docker-down all

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary
	go build -o $(BINARY) $(CMD)

run: ## Run the application
	go run $(CMD)

test: ## Run all tests
	go test ./...

fmt: ## Format code
	gofmt -s -w .

vet: ## Run go vet
	go vet ./...

lint: ## Run lints (golangci-lint)
	golangci-lint run ./...

clean: ## Remove build artifacts
	rm -f $(BINARY)
	go clean

docker-up: ## Start NATS with docker compose
	docker compose up -d

docker-down: ## Stop NATS
	docker compose down

docker-build: ## Build the Docker image
	docker build -t nephtys:latest .

all: fmt vet lint test ## Run fmt, vet, lint, and tests
