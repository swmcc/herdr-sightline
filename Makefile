VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: setup tools build test check link unlink hooks clean

# Pinned so `make check` and CI run the same versions. CI installs them with `make tools`.
STATICCHECK_VERSION := v0.8.2
GOVULNCHECK_VERSION := v1.9.0

# One time setup: the tools `make check` needs, plus the git hooks.
setup: tools hooks

# The tools `make check` needs, built with the toolchain go.mod pins.
tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# Build bin/sightline, the binary herdr calls.
build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/sightline ./cmd/sightline

test:
	go test -race ./...

# Everything that must pass before a push.
check:
	@.claude/scripts/pre-push.sh

# Build and link into your local herdr. Linking doesn't run build steps, hence the build.
link: build
	herdr plugin link .

unlink:
	herdr plugin unlink swmcc.sightline

# Run the checks for everyone who commits here, whichever tool they use.
hooks:
	@git config core.hooksPath .claude/git-hooks
	@echo "git hooks installed from .claude/git-hooks"

clean:
	rm -rf bin
