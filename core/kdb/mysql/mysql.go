// Package mysql makes kdb support database.<name>.driver: mysql. A program that uses MySQL blank-imports it in its
// main package:
//
//	import _ "github.com/go-keel/keel/core/kdb/mysql"
//
// Drivers are imported on demand so that a program using one database does not compile in the other driver.
package mysql

import (
	"database/sql/driver"

	gosql "github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/go-keel/keel/core/kdb/internal/dialect"
)

func init() {
	dialect.Register(dialect.Dialect{
		// Equivalent to sql.Open("mysql", dsn) in gorm.io/driver/mysql v1.6.0.
		Connector: func(dsn string) (driver.Connector, error) {
			c, err := gosql.ParseDSN(dsn)
			if err != nil {
				return nil, err
			}
			return gosql.NewConnector(c)
		},
		New: func(dsn string, conn gorm.ConnPool) gorm.Dialector {
			return gormmysql.New(gormmysql.Config{DSN: dsn, Conn: conn})
		},
		Attrs: func(dsn string) []attribute.KeyValue {
			attrs := []attribute.KeyValue{semconv.DBSystemNameMySQL}
			if c, err := gosql.ParseDSN(dsn); err == nil && c.Addr != "" {
				attrs = append(attrs, semconv.ServerAddress(c.Addr))
			}
			return attrs
		},
	}, "mysql")
}
