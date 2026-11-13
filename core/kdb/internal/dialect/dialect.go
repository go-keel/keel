// Package dialect is the registry between kdb and the dialect subpackages. It lives under internal so that only
// core/kdb/mysql and core/kdb/postgres can register: users can only blank-import them, not add their own.
package dialect

import (
	"database/sql/driver"

	"go.opentelemetry.io/otel/attribute"
	"gorm.io/gorm"
)

type Dialect struct {
	// Connector builds a driver connector from dsn, which kdb wraps with otelsql tracing before handing it to gorm.
	// It must build the connector the same way the gorm dialect does in its own sql.Open, otherwise changing how the
	// connection is built silently changes driver behavior.
	Connector func(dsn string) (driver.Connector, error)
	// New builds the gorm dialect from the traced connection pool. dsn is passed too: the dialect still reads
	// settings from it.
	New func(dsn string, conn gorm.ConnPool) gorm.Dialector
	// Attrs are the attributes every trace record on this connection carries (database system, server address).
	Attrs func(dsn string) []attribute.KeyValue
}

var registry = map[string]Dialect{}

// Register is called only from a dialect subpackage's init; the names are the values of database.<name>.driver.
func Register(d Dialect, names ...string) {
	for _, n := range names {
		registry[n] = d
	}
}

func Lookup(name string) (Dialect, bool) {
	d, ok := registry[name]
	return d, ok
}
