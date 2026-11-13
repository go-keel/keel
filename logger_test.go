package keel_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/go-keel/keel"
	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func newCfg(logger *conf.Logger) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{Logger: logger}}
}

// SetupLogger's only output is the process default logger, so every case asserts from slog.Default().
func TestSetupLogger_ConsoleEnabled(t *testing.T) {
	keel.SetupLogger(newCfg(&conf.Logger{Console: true, Level: "info"}))
	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info level should be enabled with console on")
	}
}

// The key fallback: console off AND no otel must NOT silently drop logs.
func TestSetupLogger_FallbackWhenNoOutputs(t *testing.T) {
	keel.SetupLogger(newCfg(&conf.Logger{Console: false, Level: "info"}))
	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("logs must not be silently dropped when no outputs configured; expected stdout fallback")
	}
}

func TestSetupLogger_NilLoggerConfig(t *testing.T) {
	// config.Logger nil → defaults applied, still functional.
	keel.SetupLogger(newCfg(nil))
	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("nil logger config should default to a working info logger")
	}
}

func TestSetupLogger_ConfiguredLevel(t *testing.T) {
	keel.SetupLogger(newCfg(&conf.Logger{Console: true, Level: "warn"}))
	if slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info enabled with logger.level=warn, want disabled")
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelWarn) {
		t.Error("warn disabled with logger.level=warn, want enabled")
	}
}

// With otel.enabled=true the otelslog output is added, and it does not need kotel.Setup to have run first to avoid
// errors: otelslog.NewHandler without an explicit provider uses the global otel.GetLoggerProvider(), which is a
// harmless no-op implementation when nothing is registered.
func TestSetupLogger_OtelEnabledAddsHandlerWithoutPanicking(t *testing.T) {
	cfg := &kconfig.Config{Config: &conf.Config{
		Logger: &conf.Logger{Console: true, Level: "info"},
		Otel:   &conf.OTEL{Enabled: true},
	}}
	keel.SetupLogger(cfg)
	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("otel-enabled config should still produce a working logger")
	}
}
