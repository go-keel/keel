package kconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

// writeConfig writes configs/config.yaml (and optional configs/config.dev.yaml)
// into a temp dir and chdirs there: the config location is fixed and cannot be overridden by an environment
// variable, so the only way for a test to point NewConfig at a temp file is to change the working directory.
func writeConfig(t *testing.T, main, dev string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o700); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs/config.yaml"), []byte(main), 0o600); err != nil {
		t.Fatalf("write main config: %v", err)
	}
	if dev != "" {
		if err := os.WriteFile(filepath.Join(dir, "configs/config.dev.yaml"), []byte(dev), 0o600); err != nil {
			t.Fatalf("write dev config: %v", err)
		}
	}
	t.Chdir(dir)
}

func TestNewConfig_Valid(t *testing.T) {
	writeConfig(t, "app:\n  name: test-svc\n  node_id: 5\n", "")
	c, err := kconfig.NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.App.GetName(); got != "test-svc" {
		t.Errorf("app.name = %q, want test-svc", got)
	}
	if got := c.App.GetNodeId(); got != 5 {
		t.Errorf("app.node_id = %d, want 5", got)
	}
}

func TestNewConfig_MissingName(t *testing.T) {
	writeConfig(t, "app:\n  node_id: 1\n", "")
	if _, err := kconfig.NewConfig(); err == nil {
		t.Error("expected error for missing app.name, got nil")
	}
}

func TestNewConfig_MissingAppBlock(t *testing.T) {
	writeConfig(t, "logger:\n  level: info\n", "")
	if _, err := kconfig.NewConfig(); err == nil {
		t.Error("expected error for missing [app] block, got nil")
	}
}

func TestNewConfig_DevOverlay(t *testing.T) {
	// config.dev.yaml overrides the base file.
	writeConfig(t,
		"app:\n  name: base-svc\n  node_id: 1\n",
		"app:\n  name: dev-svc\n",
	)
	c, err := kconfig.NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.App.GetName(); got != "dev-svc" {
		t.Errorf("app.name = %q, want dev-svc (dev overlay should win)", got)
	}
	if got := c.App.GetNodeId(); got != 1 {
		t.Errorf("app.node_id = %d, want 1 (base value retained)", got)
	}
}

// With KEEL_ENV=prod a dev overlay is no longer a warning but refuses startup; without an overlay file loading proceeds as usual.
func TestNewConfig_ProdRejectsDevOverlay(t *testing.T) {
	t.Setenv("KEEL_ENV", "prod")
	writeConfig(t, "app:\n  name: base-svc\n", "app:\n  name: dev-svc\n")
	if _, err := kconfig.NewConfig(); err == nil || !strings.Contains(err.Error(), "configs/config.dev.yaml") {
		t.Errorf("want dev overlay rejected under prod, got %v", err)
	}

	writeConfig(t, "app:\n  name: base-svc\n", "")
	if _, err := kconfig.NewConfig(); err != nil {
		t.Errorf("prod without dev overlay should load: %v", err)
	}
}

// Only dev and prod are recognized: a mistyped value must not be silently let through as dev.
func TestNewConfig_UnknownEnvRejected(t *testing.T) {
	for _, v := range []string{"production", "PROD", "test"} {
		t.Setenv("KEEL_ENV", v)
		writeConfig(t, "app:\n  name: svc\n", "")
		if _, err := kconfig.NewConfig(); err == nil || !strings.Contains(err.Error(), "KEEL_ENV") {
			t.Errorf("KEEL_ENV=%q: want rejected, got %v", v, err)
		}
	}
}

// Setting dev explicitly is the same as leaving it unset: the dev overlay takes effect as usual.
func TestNewConfig_DevEnvLoadsOverlay(t *testing.T) {
	t.Setenv("KEEL_ENV", "dev")
	writeConfig(t, "app:\n  name: base-svc\n", "app:\n  name: dev-svc\n")
	c, err := kconfig.NewConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.App.GetName(); got != "dev-svc" {
		t.Errorf("app.name = %q, want dev-svc", got)
	}
}

func TestNewConfig_LoadError(t *testing.T) {
	t.Chdir(t.TempDir()) // empty directory, configs/config.yaml does not exist
	if _, err := kconfig.NewConfig(); err == nil {
		t.Error("expected load error for missing config file, got nil")
	}
}

// 0 and unset mean "use the default". A negative value fails Validate, see TestValidate_Rules.
func TestStopTimeout(t *testing.T) {
	c := func(d *durationpb.Duration) *kconfig.Config {
		return &kconfig.Config{Config: &conf.Config{App: &conf.App{StopTimeout: d}}}
	}

	if got := c(nil).StopTimeout(); got != 20*time.Second {
		t.Errorf("nil: got %v, want 20s", got)
	}
	if got := c(durationpb.New(0)).StopTimeout(); got != 20*time.Second {
		t.Errorf("zero: got %v, want 20s", got)
	}
	if got := c(durationpb.New(5 * time.Second)).StopTimeout(); got != 5*time.Second {
		t.Errorf("explicit value: got %v, want 5s", got)
	}
}

// validConfig is a config with valid values in every section; each rule case changes one thing on top of it.
func validConfig() *conf.Config {
	return &conf.Config{
		App:  &conf.App{Name: "svc", NodeId: -1},
		Otel: &conf.OTEL{Enabled: true, Endpoint: "collector:4317"},
		Server: &conf.Server{
			Http: map[string]*conf.Server_HTTP{"default": {
				Addr: ":8080",
				Auth: &conf.Server_Auth{Enabled: true, Algorithm: "HS256", JwtSecret: "s"},
				ClientIp: &conf.Server_ClientIP{TrustedProxies: []string{
					"10.0.0.0/8", "127.0.0.1", "2001:db8::/32", "::1",
				}},
			}},
			Grpc: map[string]*conf.Server_GRPC{"default": {Addr: ":9090"}},
		},
		Client:   map[string]*conf.Client{"user": {Endpoint: "dns:///user-svc:9090"}},
		Database: map[string]*conf.Database{"default": {Driver: "mysql", Source: "dsn"}},
		Redis:    map[string]*conf.Redis{"default": {Addr: []string{"redis:6379"}}},
		Nats:     map[string]*conf.NATS{"default": {Url: "nats://nats:4222"}},
		Asynq:    map[string]*conf.Asynq{"default": {Queues: map[string]int32{"default": 1}}},
	}
}

// The rules in conf.proto are the only source of config constraints and component constructors do not repeat them:
// if any rule here stops holding, the matching value gets past startup and lands on an upstream silent fallback.
// Each case asserts which key the error names.
func TestValidate_Rules(t *testing.T) {
	if err := (&kconfig.Config{Config: validConfig()}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	neg := durationpb.New(-time.Second)
	for _, tt := range []struct {
		name   string
		mutate func(c *conf.Config)
		want   string
	}{
		{"missing app section", func(c *conf.Config) { c.App = nil }, "app: "},
		{"missing app.name", func(c *conf.Config) { c.App.Name = "" }, "app.name"},
		{"node_id above upper bound", func(c *conf.Config) { c.App.NodeId = 1024 }, "app.node_id"},
		{"node_id below lower bound", func(c *conf.Config) { c.App.NodeId = -2 }, "app.node_id"},
		{"negative stop_timeout", func(c *conf.Config) { c.App.StopTimeout = neg }, "app.stop_timeout"},
		{"unknown log format", func(c *conf.Config) { c.Logger = &conf.Logger{Format: "xml"} }, "logger.format"},
		{"unknown log level", func(c *conf.Config) { c.Logger = &conf.Logger{Level: "trace"} }, "logger.level"},
		{"uppercase log level", func(c *conf.Config) { c.Logger = &conf.Logger{Level: "INFO"} }, "logger.level"},
		{"unknown stacktrace level", func(c *conf.Config) { c.Logger = &conf.Logger{StacktraceLevel: "fatal"} }, "logger.stacktrace_level"},
		{"otel enabled without endpoint", func(c *conf.Config) { c.Otel.Endpoint = "" }, "otel: endpoint is required"},
		{"sample ratio above 1", func(c *conf.Config) { c.Otel.TraceSampleRatio = 1.5 }, "otel.trace_sample_ratio"},
		{"negative sample ratio", func(c *conf.Config) { c.Otel.TraceSampleRatio = -0.1 }, "otel.trace_sample_ratio"},
		{"negative metric interval", func(c *conf.Config) { c.Otel.MetricInterval = neg }, "otel.metric_interval"},
		{"negative log batch size", func(c *conf.Config) { c.Otel.LogBatchSize = -1 }, "otel.log_batch_size"},
		{"negative log batch interval", func(c *conf.Config) { c.Otel.LogBatchInterval = neg }, "otel.log_batch_interval"},
		{"http missing addr", func(c *conf.Config) { c.Server.Http["default"].Addr = "" }, `server.http["default"].addr`},
		{"negative http timeout", func(c *conf.Config) { c.Server.Http["default"].Timeout = neg }, `server.http["default"].timeout`},
		{"grpc missing addr", func(c *conf.Config) { c.Server.Grpc["default"].Addr = "" }, `server.grpc["default"].addr`},
		{"negative grpc timeout", func(c *conf.Config) { c.Server.Grpc["default"].Timeout = neg }, `server.grpc["default"].timeout`},
		{"invalid address in trusted proxies", func(c *conf.Config) { c.Server.Http["default"].ClientIp.TrustedProxies = []string{"proxy.local"} }, `server.http["default"].client_ip.trusted_proxies[0]`},
		{"trusted proxy CIDR with non-zero host bits", func(c *conf.Config) { c.Server.Http["default"].ClientIp.TrustedProxies = []string{"10.0.0.1/8"} }, `server.http["default"].client_ip.trusted_proxies[0]`},
		{"trusted proxy with zone", func(c *conf.Config) { c.Server.Http["default"].ClientIp.TrustedProxies = []string{"fe80::1%eth0"} }, `server.http["default"].client_ip.trusted_proxies[0]`},
		{"trusted proxies together with ignore_forwarded_for", func(c *conf.Config) { c.Server.Http["default"].ClientIp.IgnoreForwardedFor = true }, "mutually exclusive"},
		{"unknown JWT algorithm", func(c *conf.Config) { c.Server.Http["default"].Auth.Algorithm = "ES256" }, "auth.algorithm"},
		{"HS256 missing secret", func(c *conf.Config) { c.Server.Http["default"].Auth.JwtSecret = "" }, "jwt_secret is required"},
		{"RS256 missing public key file", func(c *conf.Config) { c.Server.Http["default"].Auth.Algorithm = "" }, "jwt_public_key_file is required"},
		{"client missing endpoint", func(c *conf.Config) { c.Client["user"].Endpoint = "" }, `client["user"].endpoint`},
		{"negative client timeout", func(c *conf.Config) { c.Client["user"].Timeout = neg }, `client["user"].timeout`},
		{"unknown database driver", func(c *conf.Config) { c.Database["default"].Driver = "sqlite" }, `database["default"].driver`},
		{"missing DSN", func(c *conf.Config) { c.Database["default"].Source = "" }, `database["default"].source`},
		{"negative max_open", func(c *conf.Config) { c.Database["default"].MaxOpen = -1 }, `database["default"].max_open`},
		{"negative max_idle", func(c *conf.Config) { c.Database["default"].MaxIdle = -1 }, `database["default"].max_idle`},
		{"negative max_life_time", func(c *conf.Config) { c.Database["default"].MaxLifeTime = neg }, `database["default"].max_life_time`},
		{"negative slow_threshold", func(c *conf.Config) { c.Database["default"].SlowThreshold = neg }, `database["default"].slow_threshold`},
		{"unknown database log_level", func(c *conf.Config) { c.Database["default"].LogLevel = "debug" }, `database["default"].log_level`},
		{"uppercase database log_level", func(c *conf.Config) { c.Database["default"].LogLevel = "WARN" }, `database["default"].log_level`},
		{"redis without address", func(c *conf.Config) { c.Redis["default"].Addr = nil }, `redis["default"].addr`},
		{"redis address is empty string", func(c *conf.Config) { c.Redis["default"].Addr = []string{""} }, `redis["default"].addr`},
		{"negative redis db", func(c *conf.Config) { c.Redis["default"].Db = -1 }, `redis["default"].db`},
		{"negative redis pool_size", func(c *conf.Config) { c.Redis["default"].PoolSize = -1 }, `redis["default"].pool_size`},
		{"negative redis read_timeout", func(c *conf.Config) { c.Redis["default"].ReadTimeout = neg }, `redis["default"].read_timeout`},
		{"negative redis write_timeout", func(c *conf.Config) { c.Redis["default"].WriteTimeout = neg }, `redis["default"].write_timeout`},
		{"sentinel password without master name", func(c *conf.Config) { c.Redis["default"].SentinelPassword = "p" }, "set master_name too"},
		{"nats missing url", func(c *conf.Config) { c.Nats["default"].Url = "" }, `nats["default"].url`},
		{"negative stream_max_age", func(c *conf.Config) { c.Nats["default"].StreamMaxAge = neg }, `nats["default"].stream_max_age`},
		{"negative stream_replicas", func(c *conf.Config) { c.Nats["default"].StreamReplicas = -1 }, `nats["default"].stream_replicas`},
		{"negative asynq concurrency", func(c *conf.Config) { c.Asynq["default"].Concurrency = -1 }, `asynq["default"].concurrency`},
		{"blank queue name", func(c *conf.Config) { c.Asynq["default"].Queues = map[string]int32{" ": 1} }, "queue name must not be blank"},
		{"zero queue weight", func(c *conf.Config) { c.Asynq["default"].Queues["default"] = 0 }, `asynq["default"].queues["default"]`},
		{"negative queue weight", func(c *conf.Config) { c.Asynq["default"].Queues["default"] = -1 }, `asynq["default"].queues["default"]`},
	} {
		c := validConfig()
		tt.mutate(c)
		err := (&kconfig.Config{Config: c}).Validate()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to name %q", tt.name, err, tt.want)
		}
	}
}

// With authentication disabled the credential rules do not apply: a server without authentication need not fill
// in a secret just to pass validation.
func TestValidate_AuthDisabledSkipsCredentialRules(t *testing.T) {
	c := validConfig()
	c.Server.Http["default"].Auth = &conf.Server_Auth{Algorithm: "HS256"}
	if err := (&kconfig.Config{Config: c}).Validate(); err != nil {
		t.Errorf("disabled auth should not require jwt_secret: %v", err)
	}
}

// A failed validation reports every violation at once, instead of fix one, restart, hit the next.
func TestValidate_ReportsAllViolations(t *testing.T) {
	c := validConfig()
	c.App.NodeId = 1024
	c.Redis["default"].Addr = nil
	err := (&kconfig.Config{Config: c}).Validate()
	if err == nil || !strings.Contains(err.Error(), "app.node_id") || !strings.Contains(err.Error(), `redis["default"].addr`) {
		t.Errorf("want both violations reported, got %v", err)
	}
}

// The rules must actually be wired into the load path: an invalid value in YAML is rejected by NewConfig, and the
// error names the config key.
func TestNewConfig_RejectsInvalidValues(t *testing.T) {
	writeConfig(t, "app:\n  name: test-svc\n  stop_timeout: -5s\ndatabase:\n  default:\n    driver: sqlite\n    source: x\n", "")
	_, err := kconfig.NewConfig()
	if err == nil || !strings.Contains(err.Error(), "app.stop_timeout") || !strings.Contains(err.Error(), `database["default"].driver`) {
		t.Errorf("want both keys named, got %v", err)
	}
}
