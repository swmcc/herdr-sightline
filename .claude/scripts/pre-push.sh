#!/usr/bin/env bash
# Everything that must pass before a push. Used by `make check`, the git pre-push hook
# and any agent, whichever tool it is.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

step() { printf '\n▸ %s\n' "$1"; }
missing() { printf '  %s is not installed: %s\n' "$1" "$2"; exit 1; }

step "prose"
.claude/scripts/check-prose.sh

if [ ! -f go.mod ]; then
  printf '\nNo go.mod yet, so the Go checks are skipped.\n'
  exit 0
fi

# Tools installed with `go install` land here, which isn't always on PATH.
PATH="$PATH:$(go env GOPATH)/bin"

step "gofmt"
unformatted=$(gofmt -l $(git ls-files '*.go'))
if [ -n "$unformatted" ]; then
  printf 'These files need gofmt:\n%s\n' "$unformatted"
  exit 1
fi

step "go vet"
go vet ./...

step "go test -race"
go test -race ./...

step "staticcheck"
command -v staticcheck >/dev/null || missing staticcheck "go install honnef.co/go/tools/cmd/staticcheck@latest"
staticcheck ./...

step "govulncheck"
command -v govulncheck >/dev/null || missing govulncheck "go install golang.org/x/vuln/cmd/govulncheck@latest"
govulncheck ./...

printf '\nAll checks passed.\n'
