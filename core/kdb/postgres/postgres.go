// Package postgres makes kdb support database.<name>.driver: postgres (pgx is an alias). A program that uses
// PostgreSQL blank-imports it in its main package:
//
//	import _ "github.com/go-keel/keel/core/kdb/postgres"
//
// Drivers are imported on demand so that a program using one database does not compile in the other driver.
package postgres

import (
	"context"
	"database/sql/driver"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	gormpg "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/go-keel/keel/core/kdb/internal/dialect"
)

// timeZoneMatcher is copied from gorm.io/driver/postgres v1.6.3 postgres.go.
var timeZoneMatcher = regexp.MustCompile("(time_zone|TimeZone|timezone)=(.*?)($|&| )")

// connector mirrors how gorm.io/driver/postgres v1.6.3 Dialector.Initialize connects when no Conn is given: if dsn
// sets a time zone, it becomes the session time zone and timestamps without a time zone are interpreted in it. When
// this part of upstream changes, change this too. The PreferSimpleProtocol branch is not mirrored: kdb does not
// expose that option.
func connector(dsn string) (driver.Connector, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	var opts []stdlib.OptionOpenDB
	if m := timeZoneMatcher.FindStringSubmatch(dsn); len(m) > 2 {
		config.RuntimeParams["timezone"] = m[2]
		opts = append(opts, stdlib.OptionAfterConnect(func(_ context.Context, conn *pgx.Conn) error {
			loc, err := time.LoadLocation(m[2])
			if err != nil {
				return err
			}
			conn.TypeMap().RegisterType(&pgtype.Type{
				Name:  "timestamp",
				OID:   pgtype.TimestampOID,
				Codec: &pgtype.TimestampCodec{ScanLocation: loc},
			})
			return nil
		}))
	}
	return stdlib.GetConnector(*config, opts...), nil
}

func init() {
	dialect.Register(dialect.Dialect{
		Connector: connector,
		New: func(dsn string, conn gorm.ConnPool) gorm.Dialector {
			return gormpg.New(gormpg.Config{DSN: dsn, Conn: conn})
		},
		Attrs: func(dsn string) []attribute.KeyValue {
			attrs := []attribute.KeyValue{semconv.DBSystemNamePostgreSQL}
			if c, err := pgx.ParseConfig(dsn); err == nil && c.Host != "" {
				attrs = append(attrs, semconv.ServerAddress(c.Host), semconv.ServerPort(int(c.Port)))
			}
			return attrs
		},
	}, "postgres", "pgx")
}
