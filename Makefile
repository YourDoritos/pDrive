PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build clean install uninstall test lint fmt

all: build

# Vendored upstream code is checked as-is: reformatting it would pollute every
# diff against a new upstream release. See third_party/VENDOR.md.
GOFILES := $(shell find . -name '*.go' -not -path './third_party/*')

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/pdrive ./cmd/pdrive

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w $(GOFILES)

fmtcheck:
	@out=$$(gofmt -l $(GOFILES)); if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

clean:
	rm -rf bin/

install: build
	install -Dm755 bin/pdrive $(DESTDIR)$(BINDIR)/pdrive

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/pdrive
