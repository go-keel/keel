package kconfig_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

type testConf struct {
	Name  string `json:"name"`
	Limit int    `json:"limit"`
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDynamic_Load(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "name: base\nlimit: 2\n")

	d, cleanup, err := kconfig.NewDynamic[testConf](path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if got := d.Get(); got.Name != "base" || got.Limit != 2 {
		t.Fatalf("got %+v, want name=base limit=2", got)
	}
}

// A missing path must be an error: silently returning a zero-value config would let a service with empty
// secrets/limits start as usual.
func TestDynamic_MissingPathFails(t *testing.T) {
	if _, _, err := kconfig.NewDynamic[testConf]("does/not/exist.yaml"); err == nil {
		t.Fatal("missing path must fail, not fall back to a zero-value config")
	}
}

func TestDynamic_Reload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "name: v1\n")

	d, cleanup, err := kconfig.NewDynamic[testConf](path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	writeFile(t, path, "name: v2\n")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.Get().Name == "v2" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("config not reloaded, still %+v", d.Get())
}

// Dev overlay: if <base>.dev.yaml exists in the same directory it overrides the main config; keys it does not
// mention keep the main config's values.
func TestDynamic_DevOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "name: base\nlimit: 2\n")
	writeFile(t, filepath.Join(dir, "app.dev.yaml"), "name: dev\n")

	d, cleanup, err := kconfig.NewDynamic[testConf](path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if got := d.Get(); got.Name != "dev" || got.Limit != 2 {
		t.Fatalf("got %+v, want name=dev limit=2", got)
	}
}

// KEEL_ENV=prod treats app.dev.yaml the same as config.dev.yaml: it refuses startup, not just warns.
func TestDynamic_ProdRejectsDevOverlay(t *testing.T) {
	t.Setenv("KEEL_ENV", "prod")
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "name: base\n")
	writeFile(t, filepath.Join(dir, "app.dev.yaml"), "name: dev\n")

	if _, _, err := kconfig.NewDynamic[testConf](path); err == nil || !strings.Contains(err.Error(), "app.dev.yaml") {
		t.Errorf("want dev overlay rejected under prod, got %v", err)
	}
}

// The overlay file is hot-reloaded too: it is the file edited most often during development.
func TestDynamic_DevOverlayReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	devPath := filepath.Join(dir, "app.dev.yaml")
	writeFile(t, path, "name: base\n")
	writeFile(t, devPath, "name: dev1\n")

	d, cleanup, err := kconfig.NewDynamic[testConf](path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	writeFile(t, devPath, "name: dev2\n")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.Get().Name == "dev2" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("overlay not reloaded, still %+v", d.Get())
}

// When T is a proto message it is validated against the rules on its own proto. conf.Config serves here as a message
// with rules (app.name is required), because Keel does not create protos for tests. A violation at startup is an
// error that names the config key.
func TestDynamic_ValidatesProtoRulesOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	writeFile(t, path, "app:\n  node_id: 1\n")

	_, _, err := kconfig.NewDynamic[conf.Config](path)
	if err == nil || !strings.Contains(err.Error(), "app.name") {
		t.Fatalf("invalid config must fail and name the key, got %v", err)
	}
}

// syncBuffer is a concurrency-safe log buffer: reload writes from the watch goroutine while the test goroutine reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A hot reload to invalid content keeps the last good config and logs an Error; without the log, "I changed the
// config but it did not take effect" cannot be investigated.
func TestDynamic_InvalidReloadKeepsLastGoodAndLogs(t *testing.T) {
	var buf syncBuffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	path := filepath.Join(t.TempDir(), "app.yaml")
	writeFile(t, path, "app:\n  name: good\n")

	d, cleanup, err := kconfig.NewDynamic[conf.Config](path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	writeFile(t, path, "app:\n  name: changed\n  node_id: 5000\n")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "reload failed") {
		time.Sleep(20 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "reload failed") || !strings.Contains(out, "app.node_id") {
		t.Fatalf("invalid reload must log an error naming the key, got: %s", out)
	}
	if got := d.Get().GetApp().GetName(); got != "good" {
		t.Errorf("app.name = %q, want the last good value", got)
	}
}
