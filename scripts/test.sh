#!/usr/bin/env bash
# make test: the unit tests of every module, one module at a time and without the test cache. They connect to no
# server and need no environment variable. Extra arguments go to go test; make integration passes its build tag here.
#
# Built with CGO_ENABLED=0, like the production binaries. -race is added on macOS only, where it needs no cgo; on other
# systems the tests run without the race detector.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off CGO_ENABLED=0
race=()
if [ "$(uname -s)" = Darwin ]; then race=(-race); fi
for m in $(git ls-files -co --exclude-standard '*go.mod' | xargs -n1 dirname | LC_ALL=C sort); do
	echo "==> $m"
	(cd "$m" && go test ${race[@]+"${race[@]}"} -count=1 "$@" ./...)
done
echo "tests passed"
