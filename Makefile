# seasoning — POSIX sh recipes; works with macOS make (GNU make 3.81).

SHELL := /bin/sh

MODULE  := github.com/muratgozel/seasoning
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The toolchain in go.mod is authoritative; never download another one.
GO      := GOTOOLCHAIN=local go
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version

BIN := bin/seasoning

.PHONY: all build install test lint e2e e2e-deploy e2e-live clean

all: build

build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)=$(VERSION)" -o $(BIN) ./cmd/seasoning
	@echo "built $(BIN) ($(VERSION))"

# Same layout as scripts/install.sh and `seasoning update`, with version
# `dev`: the binary goes to <versions dir>/dev and <bin dir>/seasoning is
# atomically repointed at it. Honors SEASONING_BIN_DIR,
# SEASONING_VERSIONS_DIR and XDG_DATA_HOME like the installer.
install:
	@set -eu; \
	bin_dir="$${SEASONING_BIN_DIR:-$$HOME/.local/bin}"; \
	versions_dir="$${SEASONING_VERSIONS_DIR:-$${XDG_DATA_HOME:-$$HOME/.local/share}/seasoning/versions}"; \
	mkdir -p "$$bin_dir" "$$versions_dir"; \
	versions_dir="$$(CDPATH='' cd -- "$$versions_dir" && pwd)"; \
	link="$$bin_dir/seasoning"; \
	if [ -d "$$link" ]; then echo "make install: $$link is a directory; remove it and retry" >&2; exit 1; fi; \
	staged="$$versions_dir/.seasoning-dev.$$$$.tmp"; \
	tmp_link="$$bin_dir/.seasoning.$$$$.tmp"; \
	trap 'rm -f "$$staged" "$$tmp_link"' EXIT; \
	$(GO) build -trimpath -ldflags "$(LDFLAGS)=dev" -o "$$staged" ./cmd/seasoning; \
	chmod 755 "$$staged"; \
	mv -f "$$staged" "$$versions_dir/dev"; \
	rm -f "$$tmp_link"; \
	ln -s "$$versions_dir/dev" "$$tmp_link"; \
	mv -f "$$tmp_link" "$$link"; \
	echo "installed $$link -> $$versions_dir/dev"; \
	case ":$$PATH:" in *":$$bin_dir:"*|*":$$bin_dir/:"*) ;; *) echo "note: $$bin_dir is not on your PATH";; esac; \
	echo "next: seasoning setup"

test:
	$(GO) test -race ./...

lint:
	@out="$$(gofmt -l cmd internal)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

# End-to-end check of the real binary in a throwaway sandbox (scripts/e2e.sh).
e2e: build
	sh scripts/e2e.sh

# deploy/relay.sh on a throwaway Ubuntu server: a systemd container in Docker
# (scripts/e2e-deploy/test.sh).
e2e-deploy:
	bash scripts/e2e-deploy/test.sh

# Live check with real Claude Code sessions in Docker; spends model turns on
# the account logged into the volume (scripts/e2e-live/run.sh). Never in CI.
e2e-live:
	sh scripts/e2e-live/run.sh

clean:
	rm -rf bin
