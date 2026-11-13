package kdb_test

import (
	"strings"
	"testing"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/core/kdb"
)

func cfgWithDriver(driver string) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{
		Database: map[string]*conf.Database{"default": {Driver: driver}},
	}}
}

// kdb itself imports no dialect: no dialect is registered in this package's tests, so whichever is configured must
// fail before connecting and name the subpackage to blank-import.
func TestNewNamedDBRequiresDialectImport(t *testing.T) {
	for driver, pkg := range map[string]string{
		"mysql":    "core/kdb/mysql",
		"postgres": "core/kdb/postgres",
		"pgx":      "core/kdb/postgres",
	} {
		if _, _, err := kdb.NewNamedDB(cfgWithDriver(driver), "default"); err == nil ||
			!strings.Contains(err.Error(), pkg) {
			t.Fatalf("driver %q: want error naming %s, got %v", driver, pkg, err)
		}
	}
}
