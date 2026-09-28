# Use the Go on PATH with its own GOROOT (a stale GOROOT in the environment
# breaks builds).
GO      ?= env -u GOROOT go
PKG     := github.com/Hunt4Bugs/agentsd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(VERSION)

.PHONY: build test e2e lint install-test completions snapshot clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/agentsd ./cmd/agentsd

test:
	$(GO) vet ./...
	$(GO) test -race ./...

e2e:
	$(GO) test -tags e2e -count=1 ./test/e2e/

lint:
	golangci-lint run
	shellcheck install.sh scripts/*.sh test/install/*.sh

install-test:
	sh test/install/run.sh

completions:
	sh scripts/completions.sh

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist completions
