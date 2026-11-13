//go:build integration

package postgres_test

import (
	"testing"

	"github.com/go-keel/keel/core/kdb/internal/dbtest"
	_ "github.com/go-keel/keel/core/kdb/postgres"
	"github.com/go-keel/keel/internal/integrationtest"
)

func TestRealServer(t *testing.T) {
	dbtest.Run(t, "postgres", integrationtest.Env(t, "TEST_POSTGRES_DSN"))
}
