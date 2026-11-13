//go:build integration

// In-package: provides the real NATS server to knats_internal_test.go.

package knats

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/go-keel/keel/internal/integrationtest"
)

// startTestNATS returns the real server from TEST_NATS_URL after deleting every stream on it, so each test starts
// from the same empty state as the in-process server (natsserver_test.go), and deletes them again when the test ends.
func startTestNATS(t *testing.T) string {
	t.Helper()
	url := integrationtest.Env(t, "TEST_NATS_URL")
	deleteAllStreams(t, url)
	// Leave the server empty too: other tools (a smoke run, a worker started by hand) share it, and a
	// stream left behind with test subjects fails their startup check.
	t.Cleanup(func() { deleteAllStreams(t, url) })
	return url
}

// deleteAllStreams deletes every stream on the server at url.
func deleteAllStreams(t *testing.T, url string) {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	names := js.StreamNames(ctx)
	var all []string
	for n := range names.Name() {
		all = append(all, n)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	for _, n := range all {
		if err := js.DeleteStream(ctx, n); err != nil {
			t.Fatalf("delete stream %s: %v", n, err)
		}
	}
}
