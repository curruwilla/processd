BINARY_NAME := processd
GO          := go
PKG         := github.com/curruwilla/processd/internal/version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -ldflags "-s -w \
	-X $(PKG).Version=$(VERSION) \
	-X $(PKG).Commit=$(COMMIT) \
	-X $(PKG).Date=$(DATE)"

.PHONY: all build clean test test-race test-integration cover lint lint-fix fmt vet audit tidy release-check release-snapshot release-docker install-tools help

all: fmt vet lint test build

## build: Build the processd binary into bin/
build:
	CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/processd

## clean: Remove build and coverage artifacts
clean:
	rm -rf bin/
	rm -f coverage.out coverage.html

## test: Run the test suite
test:
	$(GO) test ./...

## test-race: Run the test suite with the race detector
test-race:
	$(GO) test -race ./...

## test-integration: Run the end-to-end tests (starts real daemons and processes)
test-integration:
	$(GO) test -tags=integration -race -count=1 ./...

## cover: Run tests and write an HTML coverage report
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## lint-fix: Run golangci-lint with auto-fix
lint-fix:
	golangci-lint run --fix ./...

## fmt: Format the code
fmt:
	golangci-lint fmt ./...

## vet: Run go vet
vet:
	$(GO) vet ./...

## audit: Check dependencies for known vulnerabilities
audit:
	govulncheck ./...

## tidy: Sync go.mod and go.sum
tidy:
	$(GO) mod tidy

## release-check: Validate the GoReleaser configuration
release-check:
	goreleaser check

## release-snapshot: Build archives, packages and SBOMs into dist/, without publishing
release-snapshot:
	goreleaser release --snapshot --clean --skip=publish,sign,docker

## release-docker: Build the container image locally (needs docker buildx and qemu for arm64)
release-docker:
	goreleaser release --snapshot --clean --skip=publish,sign,sbom

## install-tools: Install the development tools used by this Makefile
install-tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GO) install github.com/goreleaser/goreleaser/v2@latest
	$(GO) install github.com/anchore/syft/cmd/syft@latest

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
