PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build clean install uninstall test lint fmt

all: build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/pdrive ./cmd/pdrive

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -rf bin/

install: build
	install -Dm755 bin/pdrive $(DESTDIR)$(BINDIR)/pdrive

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/pdrive
