.PHONY: build install build-proxy test lint

# Build the project
build:
	mkdir -p bin
	go build -o ./bin/deepai ./cmd/deepai

# install(1) writes a new file and renames it into place; `cp` overwrote the
# existing one in place, keeping its inode. On macOS that is fatal: Go's arm64
# binaries carry an ad-hoc (linker-signed) signature, the kernel caches the
# validation state per vnode, and the rewritten pages no longer match it — so
# every later exec is SIGKILLed before main runs. The shell reports it as a
# bare "killed" with no output, which looks nothing like a signing problem.
# The rename also keeps a running deepai from having its pages swapped out
# from under it mid-run.
install: build
	mkdir -p ~/.local/bin
	install -m 755 ./bin/deepai ~/.local/bin/deepai

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