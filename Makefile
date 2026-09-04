# pDrive is a per-user application — the daemon runs as you, not root, so a
# user-local prefix is the correct default and needs no sudo.
#
# pdrive-gate is the exception: fanotify permission events require
# CAP_SYS_ADMIN, so it installs system-wide via `make install-gate`.
PREFIX      ?= $(HOME)/.local
BINDIR      ?= $(PREFIX)/bin
USERUNITDIR ?= $(HOME)/.config/systemd/user

GATE_PREFIX  ?= /usr/local
GATE_BINDIR  ?= $(GATE_PREFIX)/bin
SYSUNITDIR   ?= /etc/systemd/system

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

# Vendored upstream code is checked as-is: reformatting it would pollute every
# diff against a new upstream release. See third_party/VENDOR.md.
GOFILES := $(shell find . -name '*.go' -not -path './third_party/*')

.PHONY: all build clean install install-gate uninstall uninstall-gate test lint fmt fmtcheck screenshots

all: build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/pdrive      ./cmd/pdrive
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/pdrived     ./cmd/pdrived
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/pdrivectl   ./cmd/pdrivectl
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/pdrive-gate ./cmd/pdrive-gate

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w $(GOFILES)

fmtcheck:
	@out=$$(gofmt -l $(GOFILES)); if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

# Regenerates the README images from the current UI code. Needs rsvg-convert
# and ImageMagick; only ever run by hand, never by CI.
screenshots:
	go run ./tools/screenshot -out assets
	./tools/screenshot/compose.sh assets

clean:
	rm -rf bin/

# User install: TUI, daemon, CLI, and the user systemd unit. No sudo.
install: build
	install -Dm755 bin/pdrive    $(DESTDIR)$(BINDIR)/pdrive
	install -Dm755 bin/pdrived   $(DESTDIR)$(BINDIR)/pdrived
	install -Dm755 bin/pdrivectl $(DESTDIR)$(BINDIR)/pdrivectl
	sed 's|^ExecStart=.*|ExecStart=$(BINDIR)/pdrived|' dist/pdrived.service \
		| install -Dm644 /dev/stdin $(DESTDIR)$(USERUNITDIR)/pdrived.service
	@echo
	@if [ -x "$(GATE_BINDIR)/pdrive-gate" ]; then \
		echo; \
		echo "NOTE: pdrive-gate is installed at $(GATE_BINDIR)/pdrive-gate and was"; \
		echo "      NOT updated by this target. It talks a versioned protocol with"; \
		echo "      pdrived, so update it too:"; \
		echo "          sudo make install-gate && sudo systemctl restart pdrive-gate"; \
	fi
	@echo
	@echo "Installed. Start the daemon with:"
	@echo "    systemctl --user daemon-reload"
	@echo "    systemctl --user enable --now pdrived"
	@echo "    loginctl enable-linger $$USER   # keep syncing when logged out"

# Optional, needs root: makes directory listings wait until they are current.
install-gate: build
	install -Dm755 bin/pdrive-gate $(DESTDIR)$(GATE_BINDIR)/pdrive-gate
	sed 's|^ExecStart=.*|ExecStart=$(GATE_BINDIR)/pdrive-gate|' dist/pdrive-gate.service \
		| install -Dm644 /dev/stdin $(DESTDIR)$(SYSUNITDIR)/pdrive-gate.service
	@echo
	@echo "Installed pdrive-gate. Enable it with:"
	@echo "    sudo systemctl daemon-reload"
	@echo "    sudo systemctl enable --now pdrive-gate"

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/pdrive $(DESTDIR)$(BINDIR)/pdrived $(DESTDIR)$(BINDIR)/pdrivectl
	rm -f $(DESTDIR)$(USERUNITDIR)/pdrived.service

uninstall-gate:
	-systemctl stop pdrive-gate 2>/dev/null || true
	-systemctl disable pdrive-gate 2>/dev/null || true
	rm -f $(DESTDIR)$(GATE_BINDIR)/pdrive-gate
	rm -f $(DESTDIR)$(SYSUNITDIR)/pdrive-gate.service
