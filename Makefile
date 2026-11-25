
# buf plugins are pinned in each buf.gen.yaml via go run ...@version, so no global install is needed.
# buf itself is pinned too: @latest would make different machines at different times generate different output,
# and the generated artifacts are committed.
# Done means `make check test` passes; after changing code that talks to Redis, NATS, MySQL or PostgreSQL, also
# `make integration`.

# Static checks only, no tests. See scripts/check.sh.
.PHONY: check
check:
	@scripts/check.sh

# Unit tests; no server and no environment variable needed. See scripts/test.sh.
.PHONY: test
test:
	@scripts/test.sh

# Real-server tests, unit tests included; servers and variables are listed in scripts/integration.sh.
.PHONY: integration
integration:
	@scripts/integration.sh

# Git hooks do not travel with a clone; run once per clone.
.PHONY: init-hooks
init-hooks:
	git config core.hooksPath .githooks

.PHONY: init
init:
	go install github.com/bufbuild/buf/cmd/buf@v1.75.0

# Third-party .proto files come from scripts/sync-third-party.sh (pinned, via the Go module proxy), never from the Buf
# Schema Registry; generation reads the root buf.yaml workspace.
.PHONY: config
config:
	buf generate conf --template conf/buf.gen.yaml

.PHONY: proto
proto:
	scripts/sync-third-party.sh
	buf generate proto --template proto/buf.gen.yaml

.PHONY: lint
lint:
	buf lint
	@scripts/check-doc-links.sh

.PHONY: breaking
# Check proto breaking changes against main
breaking:
	buf breaking --against '.git#branch=main'
