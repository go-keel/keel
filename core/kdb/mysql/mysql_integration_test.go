//go:build integration

package mysql_test

import (
	"testing"

	"github.com/go-keel/keel/core/kdb/internal/dbtest"
	_ "github.com/go-keel/keel/core/kdb/mysql"
	"github.com/go-keel/keel/internal/integrationtest"
)

func TestRealServer(t *testing.T) {
	dbtest.Run(t, "mysql", integrationtest.Env(t, "TEST_MYSQL_DSN"))
}
