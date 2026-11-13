package kconfig

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"buf.build/go/protovalidate"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/file"
	"google.golang.org/protobuf/proto"

	"github.com/go-keel/keel/internal/errs"
)

// Dynamic holds a hot-reloadable config value of type T.
// Get() always returns the latest loaded value and is safe for concurrent use.
//
// Usage:
//
//	d, cleanup, err := kconfig.NewDynamic[MyConf]("configs/app.yaml")
//	defer cleanup()
//
//	// anywhere in handlers/use-cases:
//	cfg := d.Get()
type Dynamic[T any] struct {
	v    atomic.Pointer[T]
	path string
	// dev is the <base>.dev.yaml dev overlay in the main config's directory; it is non-empty only if the file existed
	// at startup. The set is fixed at startup: an overlay file created while running is not picked up (restart
	// needed), because a half-working state ("takes effect along with an edit to the main file, but not when only the
	// overlay is edited") is harder to debug than not working at all.
	dev string
}

// NewDynamic loads config from path and starts watching it for changes.
// If <base>.dev.yaml exists in the same directory (app.yaml → app.dev.yaml), it is applied over the main config as
// a dev overlay and is hot-reloaded as well.
// The returned cleanup function stops the watchers.
//
// A missing main path is an error rather than a silent fallback to a zero-value config: if the business config
// cannot be read, the service should not start as usual (zero-value secrets and zero-value limits are much harder
// to debug than a failed startup). A missing overlay path is skipped.
//
// When T is a proto message, it is validated against the protovalidate rules declared on its own proto (the same
// mechanism as the framework config conf.proto): a violation at startup returns an error; a violation on hot reload
// keeps the last good config and logs an Error.
func NewDynamic[T any](path string) (*Dynamic[T], func(), error) {
	dev, err := devOverlay(path)
	if err != nil {
		return nil, nil, err
	}
	d := &Dynamic[T]{path: path, dev: dev}
	if err := d.load(); err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.watch(ctx, d.path)
	if d.dev != "" {
		d.watch(ctx, d.dev)
	}

	return d, cancel, nil
}

// devOverlay returns the path of the <base>.dev<ext> dev overlay in path's directory, or "" if it does not exist.
//
// Without prod declared, loading it logs a warning instead of staying silent: if a deployment step carries this
// file into production by mistake, dev placeholder secrets and cookie_secure=false take effect silently, and this
// Warn in the log is the only trace. A process that declares KEEL_ENV=prod refuses to start, consistent with how
// NewConfig handles config.dev.yaml.
func devOverlay(path string) (string, error) {
	prod, err := isProd()
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(path)
	dev := strings.TrimSuffix(path, ext) + ".dev" + ext
	if _, err := os.Stat(dev); err != nil {
		return "", nil
	}
	if prod {
		return "", errs.Rules.New("kconfig.dev-overlay-in-prod", fmt.Sprintf("%s exists but %s=prod", dev, envVar), "remove the dev overlay from this deployment")
	}
	slog.Warn("kconfig: dev overlay loaded", "path", dev)
	return dev, nil
}

// watch creates the first watcher synchronously so that a change made right after NewDynamic returns is not
// missed; later rebuilds happen in watchLoop.
func (d *Dynamic[T]) watch(ctx context.Context, path string) {
	w, err := file.NewSource(path).Watch()
	if err != nil {
		slog.Warn("kconfig: initial watch failed, will retry",
			"path", path, "error", err)
		w = nil
	}
	go d.watchLoop(ctx, path, w)
}

// Get returns the current config value. Safe for concurrent use.
// The returned value must be treated as read-only; it is shared across
// goroutines and replaced wholesale on reload.
func (d *Dynamic[T]) Get() *T {
	return d.v.Load()
}

func (d *Dynamic[T]) load() error {
	sources := []config.Source{file.NewSource(d.path)}
	if d.dev != "" {
		sources = append(sources, file.NewSource(d.dev))
	}

	c := config.New(config.WithSource(sources...))
	defer func() {
		if err := c.Close(); err != nil {
			slog.Warn("kconfig: close config source", "path", d.path, "error", err)
		}
	}()

	if err := c.Load(); err != nil {
		return errs.Rules.Wrap(err, "kconfig.load", fmt.Sprintf("cannot load config file %s: %v", d.path, err), "check the path passed to NewDynamic and make sure the file is readable YAML")
	}

	var val T
	if err := c.Scan(&val); err != nil {
		return errs.Rules.Wrap(err, "kconfig.scan", fmt.Sprintf("cannot decode %s into the target type: %v", d.path, err), "fix the key or value type named in the message so it matches the type passed to NewDynamic")
	}
	if m, ok := any(&val).(proto.Message); ok {
		if err := protovalidate.Validate(m); err != nil {
			return errs.Rules.Wrap(err, "kconfig.invalid", fmt.Sprintf("config file %s fails its protovalidate rules: %v", d.path, err), "fix each field the message lists so it satisfies its rule in the .proto that defines the type")
		}
	}
	d.v.Store(&val)
	return nil
}

// watchLoop consumes events from w (may be nil), rebuilding the watcher with
// exponential backoff whenever it fails.
//
// Rebuilding is not defensive redundancy: K8s updates a ConfigMap by swapping a symlink while fsnotify watches the
// old inode, so the watch inevitably breaks. Without this, hot reload under a ConfigMap mount works only once.
func (d *Dynamic[T]) watchLoop(ctx context.Context, path string, w config.Watcher) {
	const maxBackoff = time.Minute
	backoff := time.Second

	for ctx.Err() == nil {
		if w == nil {
			var err error
			w, err = file.NewSource(path).Watch()
			if err != nil {
				slog.Warn("kconfig: watch failed, retrying",
					"path", path, "backoff", backoff, "error", err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}
		backoff = time.Second
		d.consume(ctx, path, w)
		w = nil
	}
}

// consume drains watcher events until the watcher errors or ctx is canceled.
func (d *Dynamic[T]) consume(ctx context.Context, path string, w config.Watcher) {
	// Unblock w.Next() when ctx is canceled.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = w.Stop()
	}()

	for {
		if _, err := w.Next(); err != nil {
			if ctx.Err() == nil {
				slog.Warn("kconfig: watcher stopped, rebuilding",
					"path", path, "error", err)
			}
			return
		}
		// Debounce: editors may emit multiple write events per save. A fixed
		// wait lets a burst settle before reloading. Bursts are not fully
		// coalesced (config.Watcher.Next has no non-blocking drain), but reload
		// is idempotent so at worst a few extra reads happen within the window.
		time.Sleep(50 * time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		if err := d.load(); err != nil {
			slog.Error("kconfig: reload failed, keeping last good config",
				"path", path, "error", err)
		}
	}
}

// sleepCtx sleeps for d or until ctx is canceled; reports whether it slept fully.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
