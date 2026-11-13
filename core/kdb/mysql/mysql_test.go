package mysql_test

import (
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/go-keel/keel/core/kdb/internal/dialect"
)

// Blank-importing this package registers mysql; the server address in the trace attributes comes from dsn.
func TestRegistered(t *testing.T) {
	d, ok := dialect.Lookup("mysql")
	if !ok {
		t.Fatal("mysql not registered")
	}
	attrs := d.Attrs("u:p@tcp(db.local:3306)/app")
	want := semconv.ServerAddress("db.local:3306")
	if len(attrs) != 2 || attrs[0] != semconv.DBSystemNameMySQL || attrs[1] != want {
		t.Fatalf("attrs = %v", attrs)
	}
}
