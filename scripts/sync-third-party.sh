#!/usr/bin/env bash
# Copies the third-party .proto files Keel imports or prescribes into third_party/, from pinned upstream sources fetched
# through the Go module proxy (checksummed, no Buf Schema Registry). `make proto` runs it; `make check` compares the
# result with what is committed.
#
# The versions must describe the same schema as the Go packages in go.mod (buf.build/gen/go/bufbuild/protovalidate and
# google.golang.org/genproto/googleapis/api). TestThirdPartyMatchesGoPackages fails when they drift; then pick the
# upstream commit that matches the go.mod version and update it here.
set -euo pipefail
cd "$(dirname "$0")/.."

PROTOVALIDATE=github.com/bufbuild/protovalidate@v1.2.3-0.20260819171645-ec950f2039c7
GOOGLEAPIS=github.com/googleapis/googleapis@v0.0.0-20261014221013-9fe792aa92a6

moddir() {
	local dir
	dir=$(cd "$(mktemp -d)" && GOWORK=off GOFLAGS=-mod=mod go mod download -json "$1" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
	[ -n "$dir" ] || { echo "sync-third-party: cannot download $1" >&2; exit 1; }
	echo "$dir"
}

pv=$(moddir "$PROTOVALIDATE")
ga=$(moddir "$GOOGLEAPIS")

rm -rf third_party/protovalidate third_party/googleapis
mkdir -p third_party/protovalidate/buf/validate third_party/googleapis/google/api
cp "$pv/proto/protovalidate/buf/validate/validate.proto" third_party/protovalidate/buf/validate/
cp "$ga"/google/api/{annotations,http,httpbody}.proto third_party/googleapis/google/api/
chmod -R u+w third_party
