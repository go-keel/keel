package keel_test

import (
	"testing"

	"google.golang.org/grpc"

	"github.com/go-keel/keel"
	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

type fakeClient struct{ cc grpc.ClientConnInterface }

func clientCfg(clients map[string]*conf.Client) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{
		App:    &conf.App{Name: "test-svc"},
		Client: clients,
	}}
}

func newFake(cc grpc.ClientConnInterface) fakeClient { return fakeClient{cc} }

func TestNewServiceClient_InvalidParams(t *testing.T) {
	cfg := clientCfg(map[string]*conf.Client{"svc": {Endpoint: "dns:///svc:9000"}})
	cases := map[string]func() error{
		"nil config": func() error { _, _, err := keel.NewServiceClient(nil, "svc", newFake, nil); return err },
		"config without body": func() error {
			_, _, err := keel.NewServiceClient(&kconfig.Config{}, "svc", newFake, nil)
			return err
		},
		"empty service name": func() error { _, _, err := keel.NewServiceClient(cfg, "", newFake, nil); return err },
		"nil constructor":    func() error { _, _, err := keel.NewServiceClient[fakeClient](cfg, "svc", nil, nil); return err },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("err = nil, want client.invalid-params")
			}
		})
	}
}

func TestNewServiceClient_MissingConfig(t *testing.T) {
	cfg := clientCfg(map[string]*conf.Client{"other": {Endpoint: "dns:///other:9000"}})
	if _, _, err := keel.NewServiceClient(cfg, "svc", newFake, nil); err == nil {
		t.Error("missing service config should error")
	}
}

func TestNewServiceClient_LazyConstruction(t *testing.T) {
	// v3 grpc.NewClient connects lazily: construction should succeed and not block even if the endpoint is unreachable.
	cfg := clientCfg(map[string]*conf.Client{"svc": {Endpoint: "dns:///unreachable.invalid:9000"}})
	c, cleanup, err := keel.NewServiceClient(cfg, "svc", newFake, nil)
	if err != nil {
		t.Fatalf("lazy construction should not fail: %v", err)
	}
	defer cleanup()
	if c.cc == nil {
		t.Error("client conn should be set")
	}
}
