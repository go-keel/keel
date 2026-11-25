#!/usr/bin/env bash
# make check: the static checks, module by module; it runs no test (`make test`, `make integration`). It needs no
# external service and stops at the first failing step (the last "==>" line names it).
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off

GOLANGCI=github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
step() { echo "==> $*"; }

step "generated files match their sources"
snapshot() { find proto conf third_party -type f \( -name '*.pb.go' -o -name '*.proto' \) | LC_ALL=C sort | xargs cksum; }
before=$(snapshot)
make -s proto config >/dev/null
after=$(snapshot)
if [ "$before" != "$after" ]; then
	echo "Generated files or third_party/ did not match their sources (edited by hand, or a source changed without"
	echo "regenerating). They have been regenerated; review the diff and commit it:"
	diff <(echo "$before") <(echo "$after") | sed -n 's/^[<>] [0-9]* [0-9]* /  /p' | sort -u
	exit 1
fi

step "gofmt"
bad=$(gofmt -l $(git ls-files -co --exclude-standard '*.go'))
if [ -n "$bad" ]; then
	echo "$bad"
	echo "gofmt: the files above are not formatted; run gofmt -w on them"
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
GOBIN="$tmp" go install "$GOLANGCI"

for m in $(git ls-files -co --exclude-standard '*go.mod' | xargs -n1 dirname | LC_ALL=C sort); do
	step "$m: go vet (default and integration build tags)"
	(cd "$m" && go vet ./... && go vet -tags integration ./...)
	step "$m: golangci-lint"
	(cd "$m" && "$tmp/golangci-lint" run ./...)
done

step "buf lint and doc links"
make -s lint

step "English only (scripts/english-allow.list)"
scripts/check-english.sh

echo "check passed"
