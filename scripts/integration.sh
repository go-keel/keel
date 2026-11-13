#!/usr/bin/env bash
# make integration: runs the *_integration_test.go files (build tag `integration`) of every module against real
# servers, one package at a time and without the test cache, so every run talks to the servers. A test fails when its
# variable is unset. Start the servers (ports chosen to avoid the usual ones) and export the variables:
#
#   docker run -d --rm --name keel-it-redis -p 56379:6379 redis:8
#   docker run -d --rm --name keel-it-nats  -p 54222:4222 nats:2 -js
#   docker run -d --rm --name keel-it-mysql -p 53306:3306 -e MYSQL_ROOT_PASSWORD=keel -e MYSQL_DATABASE=keel_test mysql:8.4
#   docker run -d --rm --name keel-it-pg    -p 55432:5432 -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel_test postgres:17
#
#   export TEST_REDIS_ADDR=127.0.0.1:56379
#   export TEST_NATS_URL=nats://127.0.0.1:54222
#   export TEST_MYSQL_DSN='root:keel@tcp(127.0.0.1:53306)/keel_test?parseTime=true'
#   export TEST_POSTGRES_DSN='postgres://postgres:keel@127.0.0.1:55432/keel_test?sslmode=disable'
#
# The tests own these servers: they create and drop tables, delete NATS streams and change Redis settings.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
for m in $(git ls-files -co --exclude-standard '*go.mod' | xargs -n1 dirname | LC_ALL=C sort); do
	echo "==> $m"
	(cd "$m" && go test -race -tags integration -count=1 -p 1 ./...)
done
echo "integration passed"
