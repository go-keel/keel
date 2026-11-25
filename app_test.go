package keel_test

import (
	"context"
	"testing"

	"github.com/go-keel/keel"
	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

type fakeServer struct{}

func (fakeServer) Start(context.Context) error { return nil }
func (fakeServer) Stop(context.Context) error  { return nil }

// A config without an app section is reported as a rule error, not a nil dereference.
func TestNewApp_ConfigMissing(t *testing.T) {
	for name, cfg := range map[string]*kconfig.Config{
		"nil config":     nil,
		"empty config":   {},
		"no app section": {Config: &conf.Config{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := keel.NewApp(cfg, fakeServer{}); err == nil {
				t.Fatal("err = nil, want app.config-missing")
			}
		})
	}
}

func TestNewApp_NoServers(t *testing.T) {
	if _, err := keel.NewApp(&kconfig.Config{Config: &conf.Config{App: &conf.App{Name: "n"}}}); err == nil {
		t.Fatal("err = nil, want app.no-servers")
	}
}
