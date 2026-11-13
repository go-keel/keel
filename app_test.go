package keel_test

import (
	"context"
	"testing"

	"github.com/go-keel/keel"
)

type fakeServer struct{}

func (fakeServer) Start(context.Context) error { return nil }
func (fakeServer) Stop(context.Context) error  { return nil }

// With a nil config in the factory, New must return an error rather than panic on a nil dereference,
// consistent with the existing f.config.App nil check.
func TestAppFactory_NilConfigError(t *testing.T) {
	f := keel.NewAppFactory(nil)
	if _, err := f.New(fakeServer{}); err == nil {
		t.Fatal("nil config must return error, not panic")
	}
}
