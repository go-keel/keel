// In-package: replaces the hostname and PID lookups to check the fallback instance ID.

package appid

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-keel/keel/conf"
)

// InstanceID's three-level fallback (config value → hostname → name-pid) does not run in parallel: hostnameFunc/
// pidFunc are package-level variables shared with other tests.
func TestInstanceID(t *testing.T) {
	origHostname, origPid := hostnameFunc, pidFunc
	t.Cleanup(func() { hostnameFunc, pidFunc = origHostname, origPid })

	t.Run("config value wins", func(t *testing.T) {
		if got := InstanceID(&conf.App{Id: "cfg-id", Name: "svc"}); got != "cfg-id" {
			t.Errorf("got %q, want cfg-id", got)
		}
	})

	t.Run("empty config value falls back to hostname", func(t *testing.T) {
		hostnameFunc = func() (string, error) { return "host-1", nil }
		if got := InstanceID(&conf.App{Name: "svc"}); got != "host-1" {
			t.Errorf("got %q, want host-1", got)
		}
	})

	t.Run("unavailable hostname falls back to name-pid", func(t *testing.T) {
		hostnameFunc = func() (string, error) { return "", errors.New("no hostname") }
		pidFunc = func() int { return 4242 }
		if got, want := InstanceID(&conf.App{Name: "svc"}), "svc-4242"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestLeaseOwnerIsUniquePerCall: two processes with the same instance ID get different lease owners, both prefixed by
// the instance ID.
func TestLeaseOwnerIsUniquePerCall(t *testing.T) {
	app := &conf.App{Name: "svc", Id: "host-1"}
	a, b := LeaseOwner(app), LeaseOwner(app)
	if a == b {
		t.Fatalf("LeaseOwner returned %q twice, want distinct owners", a)
	}
	for _, o := range []string{a, b} {
		if !strings.HasPrefix(o, "host-1/") {
			t.Fatalf("owner = %q, want prefix %q", o, "host-1/")
		}
	}
}
