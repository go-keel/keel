#!/usr/bin/env bash
# make integration: runs the *_integration_test.go files (build tag `integration`) of every module against real
# servers, one package at a time and without the test cache, so every run talks to the servers; the unit tests run
# again along with them (scripts/test.sh). A test fails when its variable is unset. It needs four real servers, started
# however you like (a local install, a container), and their addresses in these variables, for example:
#
#   export TEST_REDIS_ADDR=127.0.0.1:6379
#   export TEST_NATS_URL=nats://127.0.0.1:4222    # JetStream enabled (nats-server -js)
#   export TEST_MYSQL_DSN='root:pass@tcp(127.0.0.1:3306)/keel_test?parseTime=true'
#   export TEST_POSTGRES_DSN='postgres://postgres:pass@127.0.0.1:5432/keel_test?sslmode=disable'
#
# The database in each DSN must exist.
#
# The tests own these servers: they create and drop tables, delete NATS streams and change Redis settings.
# The servers must be for tests only, and two runs must not use the same servers at once.
set -euo pipefail
cd "$(dirname "$0")/.."
exec scripts/test.sh -tags integration -p 1
