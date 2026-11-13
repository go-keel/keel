// Package dbtest holds the real-database checks shared by the integration tests of the kdb dialect modules
// (core/kdb/mysql, core/kdb/postgres): behaviour only a real server shows, through kdb.NewDB.
package dbtest

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
	"github.com/go-keel/keel/core/kdb"
)

type itRow struct {
	ID   uint   `gorm:"primaryKey"`
	Code string `gorm:"uniqueIndex;size:64"`
	At   time.Time
}

func (itRow) TableName() string { return "keel_it_row" }

// Run opens driver/dsn with kdb.NewDB and checks that a unique-key violation becomes gorm.ErrDuplicatedKey (kdb turns
// on TranslateError) and that an instant written in a non-UTC zone reads back as the same instant.
func Run(t *testing.T, driver, dsn string) {
	t.Helper()
	cfg := &kconfig.Config{Config: &conf.Config{
		App:      &conf.App{Name: "it"},
		Database: map[string]*conf.Database{"default": {Driver: driver, Source: dsn}},
	}}
	db, cleanup, err := kdb.NewDB(cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(cleanup)
	// Test-owned scratch table; application code never changes schema.
	if err := db.Migrator().DropTable(&itRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&itRow{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&itRow{}) })

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("UTC+8", 8*3600))
	if err := db.Create(&itRow{Code: "a", At: at}).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Create(&itRow{Code: "a", At: at}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("duplicate unique key: got %v, want gorm.ErrDuplicatedKey", err)
	}
	var got itRow
	if err := db.Where("code = ?", "a").First(&got).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.At.Equal(at) {
		t.Errorf("instant changed on round trip: wrote %v, read %v", at, got.At)
	}
}
