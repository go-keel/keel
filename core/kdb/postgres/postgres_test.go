package postgres_test

import (
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/go-keel/keel/core/kdb/internal/dialect"
)

// Blank-importing this package registers postgres and its alias pgx; the server address in the trace attributes
// comes from dsn.
func TestRegistered(t *testing.T) {
	for _, name := range []string{"postgres", "pgx"} {
		d, ok := dialect.Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		attrs := d.Attrs("postgres://u:p@db.local:5433/app")
		if len(attrs) != 3 || attrs[0] != semconv.DBSystemNamePostgreSQL ||
			attrs[1] != semconv.ServerAddress("db.local") || attrs[2] != semconv.ServerPort(5433) {
			t.Fatalf("%s attrs = %v", name, attrs)
		}
	}
}
