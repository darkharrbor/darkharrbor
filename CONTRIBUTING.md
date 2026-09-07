# Contributing to Dark Harrbor

Thank you for your interest in contributing!

## How to Contribute

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Make your changes
4. Run the test suite: `cd src && go test ./... -count=1`
5. Run the static suite: `gofmt -l . && go vet ./... && staticcheck ./...`
6. Commit your changes
7. Open a pull request

## Code Style

- Follow standard Go conventions
- Run `gofmt` before committing
- Add tests for new features
- Keep commits focused and atomic

## Reporting Issues

Open an issue at the GitHub repository with:
- Description of the bug or feature request
- Steps to reproduce (for bugs)
- Expected vs actual behavior
- Environment details (Go version, OS)

## Security

See [SECURITY.md](SECURITY.md) for responsible disclosure.
