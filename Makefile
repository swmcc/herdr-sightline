VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: setup build test check link unlink hooks clean

# One time setup: the tools `make check` needs, plus the git hooks.
setup: hooks
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest

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
