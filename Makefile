# Range — build, test and package.
#
# Works with the GNU make that ships with macOS (3.81) and with current GNU
# make on Linux. Needs Go (see go.mod) and nothing else.
#
#   make            build bin/range for this machine
#   make test       run the tests with the race detector
#   make check      what CI checks: formatting, vet, tests
#   make install    install into $(PREFIX)/bin (default ~/.local/bin)
#   make dist       release archives for macOS and Linux into dist/
#   make clean      remove bin/ and dist/

GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin

GOOS    := $(shell $(GO) env GOOS)
GOARCH  := $(shell $(GO) env GOARCH)
PKG     := ./cmd/range

# On macOS, environments run in a Linux VM, and range installs a Linux build of
# itself there. It looks for range-linux-<arch> beside its own executable, so
# every macOS build, install and archive carries that file too.
GUEST   := range-linux-$(GOARCH)

.PHONY: all build test check fmt vet install uninstall dist clean

all: build

build:
	$(GO) build -o bin/range $(PKG)
ifeq ($(GOOS),darwin)
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 $(GO) build -o bin/$(GUEST) $(PKG)
endif

test:
	$(GO) test -race ./...

fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt-ed:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

check: fmt vet test

install: build
	mkdir -p $(BINDIR)
	cp bin/range $(BINDIR)/range
ifeq ($(GOOS),darwin)
	cp bin/$(GUEST) $(BINDIR)/$(GUEST)
endif
	@echo "installed range into $(BINDIR)"

uninstall:
	rm -f $(BINDIR)/range $(BINDIR)/range-linux-amd64 $(BINDIR)/range-linux-arm64

# Archives are named after `uname -s` and `uname -m`, so a one-line install
# can pick the right one without a lookup table. The Apple silicon build uses
# cgo, for Virtualization.framework, so it is built on a Mac:
#
#   range_Darwin_arm64.tar.gz   range_Darwin_x86_64.tar.gz
#   range_Linux_aarch64.tar.gz  range_Linux_x86_64.tar.gz
dist:
	rm -rf dist && mkdir -p dist
	@set -e; \
	for target in darwin/arm64/Darwin/arm64 darwin/amd64/Darwin/x86_64 \
	              linux/arm64/Linux/aarch64 linux/amd64/Linux/x86_64; do \
		os=$$(echo $$target | cut -d/ -f1); arch=$$(echo $$target | cut -d/ -f2); \
		name=range_$$(echo $$target | cut -d/ -f3)_$$(echo $$target | cut -d/ -f4); \
		dir=dist/$$name; mkdir -p $$dir; \
		echo "building $$name"; \
		cgo=0; [ $$os/$$arch = darwin/arm64 ] && cgo=1; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=$$cgo $(GO) build -trimpath -o $$dir/range $(PKG); \
		if [ $$os = darwin ]; then \
			GOOS=linux GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -o $$dir/range-linux-$$arch $(PKG); \
		fi; \
		cp LICENSE NOTICE README.md $$dir/; \
		tar -C $$dir -czf dist/$$name.tar.gz .; \
		rm -rf $$dir; \
	done
	cd dist && (shasum -a 256 *.tar.gz 2>/dev/null || sha256sum *.tar.gz) > SHA256SUMS
	@cat dist/SHA256SUMS

clean:
	rm -rf bin dist
