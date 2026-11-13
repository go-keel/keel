package kdb

import (
	"context"
	"fmt"
	"time"

	"github.com/XSAM/otelsql"
	"gorm.io/gorm"

	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/core/kdb/internal/dialect"
	"github.com/go-keel/keel/internal/errs"
)

// pingTimeout bounds the startup connectivity probe. It matches kredis/knats: if a connection cannot be made within
// 5s, the network needs fixing, not the timeout, so it is not configurable.
const pingTimeout = 5 * time.Second

type DB struct {
	*gorm.DB
}

// NewDB opens the "default" database. The returned cleanup closes the
// underlying connection pool.
func NewDB(config *kconfig.Config) (*DB, func(), error) {
	return NewNamedDB(config, "default")
}

func NewNamedDB(config *kconfig.Config, name string) (*DB, func(), error) {
	if len(config.Database) == 0 {
		return nil, nil, errs.Rules.New("kdb.config-missing", "the config has no `database` section", "add `database: {default: {driver: ..., source: ...}}` to the config")
	}
	if name == "" {
		name = "default"
	}
	c := config.Database[name]
	if c == nil {
		return nil, nil, errs.Rules.New("kdb.config-missing", fmt.Sprintf("the config has no database.%s section", name), fmt.Sprintf("add `database: {%s: {driver: ..., source: ...}}` to the config or fix the name passed to NewNamedDB", name))
	}

	// Dialects are registered by core/kdb/mysql and core/kdb/postgres in init, and kdb itself imports no driver: a
	// program that uses one database need not compile in the other. A missing blank import fails here at startup
	// and names the package to import.
	d, ok := dialect.Lookup(c.Driver)
	if !ok {
		return nil, nil, errs.Rules.New("kdb.driver-missing",
			fmt.Sprintf("database.%s.driver is %q but that driver is not imported", name, c.Driver),
			fmt.Sprintf("go get github.com/go-keel/keel/core/kdb/%[1]s and add `import _ \"github.com/go-keel/keel/core/kdb/%[1]s\"` to package main", importName(c.Driver)))
	}

	// Tracing is done at the database/sql layer by otelsql: it is driver-agnostic, so unlike a tracing plugin that
	// reads the server address by gorm dialect type it does not compile in every driver. Tracing and metrics both use
	// otel's globals (a process facility, ADR keel-0004): without kotel.Setup or with otel.enabled=false they are
	// harmless no-ops. Query parameter values do not go into traces; only the statement with placeholders is recorded.
	//
	// otelsql records one span per action on a connection, so by default a single insert records six or seven. The
	// kinds turned off carry no debugging information and only take up trace storage: session reset before the
	// connection returns to the pool, prepare (the execute right after it carries the same SQL), and result set
	// iteration. The driver signals "retry with prepare" with ErrSkip (with MySQL and interpolateParams off, every SQL
	// with parameters goes through this first), and neither the trace nor the duration metric records it as an error:
	// these are two independent switches in otelsql v0.44.0, and turning off only the tracing one would make
	// successful queries carry error.type in metrics, polluting the failure rate.
	conn, err := d.Connector(c.Source)
	if err != nil {
		return nil, nil, errs.Rules.Wrap(err, "kdb.source-invalid", fmt.Sprintf("database.%s.source is not a valid %s DSN: %v", name, c.Driver, err), fmt.Sprintf("fix database.%s.source so it parses as a %s DSN", name, c.Driver))
	}
	sqlDB := otelsql.OpenDB(conn,
		otelsql.WithAttributes(d.Attrs(c.Source)...),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			OmitConnResetSession: true,
			OmitConnPrepare:      true,
			OmitRows:             true,
			DisableErrSkip:       true,
		}),
		otelsql.WithDisableSkipErrMeasurement(true),
	)
	cleanup := func() { _ = sqlDB.Close() }

	if c.MaxIdle > 0 {
		sqlDB.SetMaxIdleConns(int(c.MaxIdle))
	} else {
		sqlDB.SetMaxIdleConns(10)
	}
	if c.MaxOpen > 0 {
		sqlDB.SetMaxOpenConns(int(c.MaxOpen))
	} else {
		sqlDB.SetMaxOpenConns(100)
	}
	sqlDB.SetConnMaxLifetime(durationOr(c.GetMaxLifeTime(), 5*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err = sqlDB.PingContext(ctx); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("kdb: ping %s (name=%s): %w", c.Driver, name, err)
	}

	// TranslateError makes drivers translate constraint errors into gorm's sentinel errors (unique key →
	// gorm.ErrDuplicatedKey, foreign key → gorm.ErrForeignKeyViolated and so on), so users detect a duplicate key with
	// errors.Is without importing a driver package or knowing dialect error codes such as MySQL 1062 /
	// PostgreSQL 23505.
	//
	// Cost: the MySQL driver drops the original error after translating and leaves only the bare sentinel, so which
	// key and which value collided cannot be read from the error, nor from kdb's slog log (the log receives the
	// translated error). The PostgreSQL driver wraps the original error with %w and does not have this problem.
	//
	// This mirrors upstream behavior; when upstream changes, revisit this (versions from go.mod):
	//   - gorm.io/driver/mysql v1.6.0 error_translator.go: translates only 1062 → ErrDuplicatedKey and
	//     1451 / 1452 → ErrForeignKeyViolated, returns the bare sentinel, and returns other codes unchanged;
	//   - gorm.io/driver/postgres v1.6.3 error_translator.go: translates 23505 → ErrDuplicatedKey,
	//     23503 → ErrForeignKeyViolated, 23514 → ErrCheckConstraintViolated and
	//     42703 → ErrInvalidField, wrapping the original error with `%w: %w`;
	//   - gorm.io/gorm v1.31.2 calls the dialect's Translate in DB.AddError, so only paths that go through AddError
	//     are covered: Create / First / Find / Updates / Delete / Exec and Raw(...).Find. Scan / Row() / Rows() go
	//     through callbacks/row.go, which assigns the driver error directly to db.Error and bypasses AddError:
	//     whether preceded by a chained query (db.Table(..).Where(..).Scan) or Raw, nothing is translated and the
	//     caller still gets the driver error (*mysql.MySQLError under MySQL).
	gormCfg := &gorm.Config{Logger: newSlogLogger(c), TranslateError: true}

	// Given an existing connection pool, gorm no longer calls sql.Open itself (Initialize in gorm.io/driver/mysql
	// v1.6.0 and postgres v1.6.3 uses Conn directly when it is non-nil), so the pool settings and tracing apply to
	// every SQL statement gorm runs.
	db, err := gorm.Open(d.New(c.Source, sqlDB), gormCfg)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("kdb: open %s (name=%s): %w", c.Driver, name, err)
	}

	return &DB{DB: db}, cleanup, nil
}

// importName is the dialect subpackage name for a driver value (pgx is an alias for postgres).
func importName(driver string) string {
	if driver == "pgx" {
		return "postgres"
	}
	return driver
}
