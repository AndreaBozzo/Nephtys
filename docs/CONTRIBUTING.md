# Contributing to Nephtys

Thank you for considering contributing to Nephtys! This document provides guidelines for contributing to the project.

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/yourusername/Nephtys.git`
3. Create a feature branch: `git checkout -b feature/your-feature-name`
4. Make your changes
5. Run tests: `make all`
6. Commit your changes: `git commit -m "Add your feature"`
7. Push to your fork: `git push origin feature/your-feature-name`
8. Open a Pull Request

## Development Setup

```bash
# Start NATS with JetStream
docker compose up -d

# Configure environment
cp .env.example .env

# Run tests
make test

# Run with debug logging
NEPHTYS_LOG_LEVEL=debug go run ./cmd/nephtys
```

## Code Style

- Follow Go standard formatting: `gofmt -s -w .`
- Ensure vet passes: `go vet ./...`
- Write tests for new functionality
- Document exported types and functions

## Commit Messages

- Use clear, descriptive commit messages
- Start with a verb in imperative mood: "Add", "Fix", "Update", "Remove"
- Reference issues when applicable: "Fix #123"

## Pull Request Process

1. Update documentation for any changed functionality
2. Ensure all tests pass
3. Update the README.md if needed
4. Your PR will be reviewed by maintainers
5. Address any requested changes
6. Once approved, your PR will be merged

## Areas for Contribution

- **Connectors**: Add support for new stream sources (SSE, webhooks, gRPC)
- **Pipeline**: Implement middlewares for filtering, dedup, enrichment
- **Documentation**: Improve guides, examples, and API docs
- **Tests**: Increase test coverage
- **Bug fixes**: Fix issues listed in GitHub Issues

## Questions?

Feel free to open an issue for questions or discussions about contributing.

## Code of Conduct

Please be respectful and constructive in all interactions. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
