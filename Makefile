.PHONY: build build-proxy test lint

# Build the project
build:
	mkdir -p bin
	go build -o ./bin/deepai ./cmd/deepai

install: build
	cp ./bin/deepai ~/.local/bin/deepai

# Build the proxy server
build-proxy:
	mkdir -p bin
	go build -o ./bin/proxy ./cmd/proxy

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