package khttpclient_test

import (
	"errors"
	"testing"

	"github.com/go-keel/keel/core/khttpclient"
)

func TestAsError_Envelope(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	_, err := cli.R().Get("/conflict")
	info, ok := khttpclient.AsError(err)
	if !ok {
		t.Fatalf("AsError ok = false for %v", err)
	}
	if info.Code != 409 || info.Reason != "TODO_CONFLICT" || info.Message != "already exists" || info.Metadata["id"] != "1" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestAsError_Nil(t *testing.T) {
	if _, ok := khttpclient.AsError(nil); ok {
		t.Fatal("AsError(nil) should be ok=false")
	}
}

func TestAsError_PlainError(t *testing.T) {
	info, ok := khttpclient.AsError(errors.New("dial tcp: connection refused"))
	if !ok || info.Code != 500 || info.Reason != "" {
		t.Fatalf("plain error should map to code=500, empty reason: %+v ok=%v", info, ok)
	}
}

func TestIsReason(t *testing.T) {
	srv := newEnvelopeServer(t)
	cli := newTestClient(t, srv.URL)

	_, err := cli.R().Get("/conflict")
	if !khttpclient.IsReason(err, "TODO_CONFLICT") {
		t.Fatalf("IsReason should match TODO_CONFLICT: %v", err)
	}
	if khttpclient.IsReason(err, "OTHER") {
		t.Fatal("IsReason should not match OTHER")
	}
	if khttpclient.IsReason(nil, "TODO_CONFLICT") {
		t.Fatal("IsReason(nil, ...) should be false")
	}
	if khttpclient.IsReason(err, "") {
		t.Fatal(`IsReason(err, "") should be false`)
	}
}
