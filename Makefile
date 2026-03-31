.PHONY: build test lint

# Build the project
build:
	go build -o ./bin/deepai ./cmd/deepai

# Run tests
test:
	go test -v ./...

# Run linter
lint:
	golangci-lint run ./...
