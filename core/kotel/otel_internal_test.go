// In-package: checks how the otel config is parsed into defaults, which Setup does not return.

package kotel

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdlog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/core/kconfig"
)

func newCfg(otel *conf.OTEL) *kconfig.Config {
	return &kconfig.Config{Config: &conf.Config{
		App:  &conf.App{Name: "test-svc", Version: "v1"},
		Otel: otel,
	}}
}

func TestParseConfig_Disabled(t *testing.T) {
	for _, otel := range []*conf.OTEL{nil, {Enabled: false}} {
		if c := parseConfig(newCfg(otel)); c.Enabled {
			t.Error("should be disabled")
		}
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	c := parseConfig(newCfg(&conf.OTEL{Enabled: true, Endpoint: "collector:4317"}))
	if c.TraceSampleRatio != 1.0 {
		t.Errorf("ratio default = %v, want 1.0", c.TraceSampleRatio)
	}
	if c.MetricInterval != 10*time.Second {
		t.Errorf("metric interval default = %v, want 10s", c.MetricInterval)
	}
	if c.LogBatchSize != 512 {
		t.Errorf("log batch size default = %d, want 512", c.LogBatchSize)
	}
	if c.LogBatchInterval != time.Second {
		t.Errorf("log batch interval default = %v, want 1s", c.LogBatchInterval)
	}
	if c.ServiceName != "test-svc" || c.ServiceVersion != "v1" {
		t.Errorf("service identity mismatch: %s/%s", c.ServiceName, c.ServiceVersion)
	}
}

func TestParseConfig_ExplicitValues(t *testing.T) {
	c := parseConfig(newCfg(&conf.OTEL{
		Enabled:          true,
		Endpoint:         "collector:4317",
		TraceSampleRatio: 0.25,
		MetricInterval:   durationpb.New(30 * time.Second),
		LogBatchSize:     1024,
		LogBatchInterval: durationpb.New(5 * time.Second),
	}))
	if c.TraceSampleRatio != 0.25 {
		t.Errorf("ratio = %v, want 0.25", c.TraceSampleRatio)
	}
	if c.MetricInterval != 30*time.Second {
		t.Errorf("metric interval = %v, want 30s", c.MetricInterval)
	}
	if c.LogBatchSize != 1024 {
		t.Errorf("log batch size = %d, want 1024", c.LogBatchSize)
	}
	if c.LogBatchInterval != 5*time.Second {
		t.Errorf("log batch interval = %v, want 5s", c.LogBatchInterval)
	}
}

// 0 means "unset" and falls back to sampling everything; out-of-range values (<0 or >1) are rejected by the rules
// in conf.proto.
func TestParseConfig_ZeroRatioMeansUnset(t *testing.T) {
	c := parseConfig(newCfg(&conf.OTEL{Enabled: true, Endpoint: "c:4317", TraceSampleRatio: 0}))
	if c.TraceSampleRatio != 1.0 {
		t.Errorf("ratio 0 should fall back to 1.0, got %v", c.TraceSampleRatio)
	}
}

// With otel.enabled=false Setup touches no global and only returns a cleanup function that is safe to call.
func TestSetup_Disabled(t *testing.T) {
	cleanup, err := Setup(newCfg(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cleanup()
}

// With enabled=true Setup is the only place in the repo that sets the otel globals; if it is wrong, every component's
// otel.GetTracerProvider() and friends silently get the default delegating implementation instead of the real SDK
// provider. A test asserting the globals replaces them temporarily, restores them afterwards, and does not run in
// parallel with other tests (a process has only one set of globals).
func TestSetup_EnabledRegistersGlobals(t *testing.T) {
	oldTP, oldMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	oldLP := otel.GetLoggerProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetMeterProvider(oldMP)
		otel.SetLoggerProvider(oldLP)
	})

	// The exporter connects lazily (see docs/observability.md "Two deliberate choices"), so Setup succeeds
	// even though nothing listens on this address.
	cleanup, err := Setup(newCfg(&conf.OTEL{Enabled: true, Endpoint: "127.0.0.1:1", Insecure: true}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("TracerProvider not registered globally, got %T", otel.GetTracerProvider())
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Errorf("MeterProvider not registered globally, got %T", otel.GetMeterProvider())
	}
	if _, ok := otel.GetLoggerProvider().(*sdlog.LoggerProvider); !ok {
		t.Errorf("LoggerProvider not registered globally, got %T", otel.GetLoggerProvider())
	}
}
