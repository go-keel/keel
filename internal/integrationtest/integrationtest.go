// Package integrationtest gives integration tests (files *_integration_test.go with `//go:build integration`, run by
// `make integration`) the address of a real server. See scripts/integration.sh for the servers and variables.
package integrationtest

import (
	"os"
	"testing"
)

// Env returns the environment variable name, failing the test when it is unset: an integration test that skips
// without a server would report a pass that tested nothing.
func Env(t testing.TB, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set. Integration tests need real servers: start them and export the variables as listed "+
			"in scripts/integration.sh", name)
	}
	return v
}
