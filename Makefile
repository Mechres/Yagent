# Yagent — build helpers

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test vet race version doctor install

# Install location. Override with PREFIX=/usr/local or DESTDIR=/stage for
# system-wide installs (needs write perms on the target).
PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin

install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 yagent $(DESTDIR)$(BINDIR)/yagent
	@echo "installed yagent -> $(DESTDIR)$(BINDIR)/yagent"

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
