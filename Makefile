# Yagent — build helpers

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test vet race version doctor

build:
	go build $(LDFLAGS) -o yagent ./cmd/yagent

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

version:
	@echo $(VERSION)

doctor:
	go run ./cmd/yagent doctor
