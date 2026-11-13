package kconfig

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"buf.build/go/protovalidate"
	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/internal/errs"
)

// configPath / devConfigPath are the fixed locations of the main config and the dev overlay, relative to the
// process working directory.
//
// No environment variable overrides the main path: the overlay name config.dev.yaml is hard-coded, so once the main
// path is renamed the two no longer match (main file prod.yaml, overlay still looked up as config.dev.yaml). A
// variable path has its own cost too: "which config did this machine actually read" can only be answered by digging
// through the process environment. Deployments mount the real config under configs/ in the working directory.
const (
	configPath    = "configs/config.yaml"
	devConfigPath = "configs/config.dev.yaml"
)

// envPrefix limits which environment variables may override the config. Only variables with the KEEL_ prefix take
// part, so unrelated process environment variables (PATH/HOME/...) cannot pollute or accidentally override config
// keys.
//
// Note: the default kratos decoder splits nested keys on ".", and shell environment variable names cannot contain
// ".", so an environment variable cannot override a nested field such as server.auth.jwt_secret by path. A nested
// field is either written as a ${JWT_SECRET} placeholder in config.yaml and filled indirectly by KEEL_JWT_SECRET
// (kratos resolves placeholders by default, see docs/config.md), or the deployment writes the real value
// directly into config.yaml.
const envPrefix = "KEEL"

// envVar declares the environment the process runs in. Only dev and prod are recognized; unset means dev. With prod,
// a dev overlay (config.dev.yaml) in the working directory refuses startup instead of only logging a warning.
//
// Only these two values are accepted and anything else is an error: someone who writes production / PROD would
// believe they are protected, and "silently not taking effect" is exactly the failure this safeguard exists to
// prevent.
const envVar = "KEEL_ENV"

// Config holds the application's configuration.
type Config struct {
	*conf.Config
}

// NewConfig loads configs/config.yaml, the optional dev overlay and KEEL_-prefixed environment variables, then
// validates the result against the rules in conf/conf.proto.
func NewConfig() (*Config, error) {
	sources, err := buildSources()
	if err != nil {
		return nil, err
	}
	c := config.New(config.WithSource(sources...))
	if err := c.Load(); err != nil {
		return nil, errs.Rules.Wrap(err, "kconfig.load", fmt.Sprintf("cannot load config file %s: %v", configPath, err), "run the process from the directory that holds configs/config.yaml and make sure the file is readable YAML")
	}

	var cc conf.Config
	if err := c.Scan(&cc); err != nil {
		return nil, errs.Rules.Wrap(err, "kconfig.scan", fmt.Sprintf("cannot decode %s into the conf.Config schema: %v", configPath, err), "fix the key or value type named in the message so it matches conf/conf.proto")
	}

	cfg := &Config{Config: &cc}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultStopTimeout is the fallback when app.stop_timeout is unset. Several components derive the same deadline from
// it (keel.AppFactory's kratos.StopTimeout, the asynq.Config.ShutdownTimeout inside kasynq.WorkerServer), and all of
// them must go through the single StopTimeout() function rather than each writing its own default; otherwise the
// promise "there is only one number to configure" silently diverges somewhere again.
const defaultStopTimeout = 20 * time.Second

// StopTimeout converts app.stop_timeout into the time.Duration actually used. 0 and unset mean "use the default";
// a negative value fails Validate (the rule in conf.proto).
func (c *Config) StopTimeout() time.Duration {
	if t := c.App.GetStopTimeout().AsDuration(); t > 0 {
		return t
	}
	return defaultStopTimeout
}

// buildSources assembles the ordered sources of the main config: the base config file, the optional
// config.dev.yaml dev overlay, and KEEL_-prefixed environment variables (highest priority).
func buildSources() ([]config.Source, error) {
	prod, err := isProd()
	if err != nil {
		return nil, err
	}

	sources := []config.Source{
		file.NewSource(configPath),
	}

	// Dev overlay. Without prod declared, loading it logs a warning instead of staying silent: if a deployment step
	// carries this file into production by mistake, the dev placeholder secrets take effect silently, and this Warn in
	// the log is the only trace. A process that declares prod refuses to start.
	if _, err := os.Stat(devConfigPath); err == nil {
		if prod {
			return nil, errs.Rules.New("kconfig.dev-overlay-in-prod", fmt.Sprintf("%s exists but %s=prod", devConfigPath, envVar), "remove the dev overlay from this deployment")
		}
		slog.Warn("kconfig: dev overlay loaded", "path", devConfigPath)
		sources = append(sources, file.NewSource(devConfigPath))
	}

	// Environment variable overrides (KEEL_ prefix only).
	sources = append(sources, env.NewSource(envPrefix))
	return sources, nil
}

// isProd reads KEEL_ENV: prod is true, dev and unset are false, any other value is an error. The dev overlay handling
// of both entry points, NewConfig and NewDynamic, shares this one check.
func isProd() (bool, error) {
	switch e := os.Getenv(envVar); e {
	case "", "dev":
		return false, nil
	case "prod":
		return true, nil
	default:
		return false, errs.Rules.New("kconfig.env-invalid", fmt.Sprintf("%s=%q is not recognized", envVar, e), fmt.Sprintf("set %s to dev or prod, or leave it unset for dev", envVar))
	}
}

// Validate checks the whole config against the rules declared in conf.proto and reports every violation at once.
//
// Constraints on config values are written only in conf.proto, and NewConfig validates them here after loading.
// Component constructors do not repeat these checks; they only check assembly (whether the section they need exists,
// whether the provider matches the config). Callers that build &Config{Config: ...} directly instead of going
// through NewConfig (tests) must call it themselves.
func (c *Config) Validate() error {
	if c.Config == nil {
		return errs.Rules.New("kconfig.empty", "kconfig.Config has no conf.Config inside (Config is nil)", "build the config with kconfig.NewConfig, or set Config before calling Validate")
	}
	if err := protovalidate.Validate(c.Config); err != nil {
		return errs.Rules.Wrap(err, "kconfig.invalid", fmt.Sprintf("config fails the rules in conf/conf.proto: %v", err), "fix each field the message lists so it satisfies its rule in conf/conf.proto")
	}
	return nil
}
