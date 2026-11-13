package keel_test

import (
	"testing"

	"google.golang.org/grpc"

	"github.com/go-keel/keel"
	"github.com/go-keel/keel/conf"
)

type fakeClient struct{ cc grpc.ClientConnInterface }

func newFactory(clients map[string]*conf.Client) *keel.ServiceClientFactory {
	cfg := &keel.Config{Config: &conf.Config{
		App:    &conf.App{Name: "test-svc"},
		Client: clients,
	}}
	return keel.NewServiceClientFactory(cfg)
}

func TestNewServiceClient_InvalidParams(t *testing.T) {
	f := newFactory(nil)
	if _, _, err := keel.NewServiceClient(nil, "svc", func(cc grpc.ClientConnInterface) fakeClient { return fakeClient{cc} }, nil); err == nil {
		t.Error("nil factory should error")
	}
	if _, _, err := keel.NewServiceClient(f, "", func(cc grpc.ClientConnInterface) fakeClient { return fakeClient{cc} }, nil); err == nil {
		t.Error("empty service name should error")
	}
	if _, _, err := keel.NewServiceClient[fakeClient](f, "svc", nil, nil); err == nil {
		t.Error("nil constructor should error")
	}
	// When the factory is built with NewServiceClientFactory(nil) its internal config is nil;
	// it must return an error rather than panic on the ClientDefault/Client map dereference.
	nilCfgFactory := keel.NewServiceClientFactory(nil)
	if _, _, err := keel.NewServiceClient(nilCfgFactory, "svc", func(cc grpc.ClientConnInterface) fakeClient { return fakeClient{cc} }, nil); err == nil {
		t.Error("factory with nil config should error")
	}
}

func TestNewServiceClient_MissingConfig(t *testing.T) {
	f := newFactory(map[string]*conf.Client{"other": {Endpoint: "dns:///other:9000"}})
	_, _, err := keel.NewServiceClient(f, "svc", func(cc grpc.ClientConnInterface) fakeClient { return fakeClient{cc} }, nil)
	if err == nil {
		t.Error("missing service config should error")
	}
}

func TestNewServiceClient_LazyConstruction(t *testing.T) {
	// v3 grpc.NewClient connects lazily: construction should succeed and not block even if the endpoint is unreachable.
	f := newFactory(map[string]*conf.Client{"svc": {Endpoint: "dns:///unreachable.invalid:9000"}})
	c, cleanup, err := keel.NewServiceClient(f, "svc", func(cc grpc.ClientConnInterface) fakeClient { return fakeClient{cc} }, nil)
	if err != nil {
		t.Fatalf("lazy construction should not fail: %v", err)
	}
	defer cleanup()
	if c.cc == nil {
		t.Error("client conn should be set")
	}
}

func TestServiceClientFactory_Accessors(t *testing.T) {
	f := newFactory(nil)
	if f.Config() == nil {
		t.Error("Config() nil")
	}
}
