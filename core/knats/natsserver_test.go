//go:build !integration

// In-package: provides the in-process NATS server to knats_internal_test.go.

package knats

import (
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
)

// startTestNATS starts an in-process NATS server with JetStream and returns its URL. It drives the code under test
// (publish → subscribe → retry → give up, shutdown order); `make integration` runs the same tests against a real
// server instead (natsserver_integration_test.go).
func startTestNATS(t *testing.T) string {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("test nats server did not become ready")
	}
	return srv.ClientURL()
}
