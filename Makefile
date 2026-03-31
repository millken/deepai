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

upstream:
# git remote add upstream https://github.com/millken/deepai
# git checkout -b upstream
	git checkout upstream
	git pull upstream
	git push origin upstream
	git checkout main