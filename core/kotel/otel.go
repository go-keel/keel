package kotel

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdlog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/go-keel/keel/core/kconfig"
)

type otelConfig struct {
	Enabled          bool
	ServiceName      string
	ServiceVersion   string
	HostName         string
	Endpoint         string
	Insecure         bool
	TraceSampleRatio float64
	MetricInterval   time.Duration
	LogBatchSize     int
	LogBatchInterval time.Duration
}

// Setup is the single assembly point for the three observability signals: traces, metrics and logs. All three are
// process facilities (the "process facility" entry in GLOSSARY.md): Setup sets them once in otel's global registry
// (otel.SetTracerProvider / SetMeterProvider / otel.SetLoggerProvider), and components take them directly via
// otel.GetTracerProvider() and friends; constructors neither receive them nor have parameters for them.
//
// The globals inside the otel package hand over lazily (go.opentelemetry.io/otel/internal/global and
// otel/log/internal/global): whoever calls otel.Tracer(name) first gets a placeholder delegate, and when the real
// Provider is registered here later, the Tracer/Logger obtained earlier is taken over in place instead of staying a
// no-op forever, so constructing components before this call is fine.
//
// It returns only a cleanup function and an error: there is no provider for the caller to hold, so this is not a
// node in the dependency graph (see docs/assembly.md "Process facilities stay out of the graph").
func Setup(config *kconfig.Config) (func(), error) {
	ctx := context.Background()

	c := parseConfig(config)

	// If the config does not enable OpenTelemetry, no global is set: the delegating Provider built into the otel
	// package is already a no-op that is safe to call.
	if !c.Enabled {
		return func() {}, nil
	}

	// Shared resource attributes.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(c.ServiceName),
			semconv.ServiceVersion(c.ServiceVersion),
			semconv.HostName(c.HostName),
		),
	)
	if err != nil {
		return nil, err
	}

	// Build every provider first and register the globals (otel.Set*) only at the end: on a failure midway, the
	// parts already built are shut down, so the globals never point at a half-initialized state and no batcher
	// goroutine leaks.
	sdCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}

	// ---------- Trace ----------
	traceOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(c.Endpoint),
		otlptracegrpc.WithTimeout(10 * time.Second),
	}
	if c.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}
	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.TraceSampleRatio))),
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)

	// ---------- Metrics ----------
	metricOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(c.Endpoint),
		otlpmetricgrpc.WithTimeout(10 * time.Second),
	}
	if c.Insecure {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		shCtx, cancel := sdCtx()
		defer cancel()
		_ = tp.Shutdown(shCtx)
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(c.MetricInterval))),
		sdkmetric.WithResource(res),
	)

	// ---------- Logs ----------
	logOpts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(c.Endpoint),
		otlploggrpc.WithTimeout(10 * time.Second),
	}
	if c.Insecure {
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}
	logExp, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		shCtx, cancel := sdCtx()
		defer cancel()
		_ = tp.Shutdown(shCtx)
		_ = mp.Shutdown(shCtx)
		return nil, err
	}
	// The queue limit goes together with the batch size and interval: logs enter the queue first and are exported in
	// batches by batch size or interval; when the collector is slow or unreachable, the queue backs up to 4096
	// records and then starts dropping the oldest. The queue limit is not configurable: it only chooses between
	// "drop logs or grow memory", and that choice has no second reasonable answer; to drop less, lower the batch
	// interval.
	logProcessor := sdlog.NewBatchProcessor(
		logExp,
		sdlog.WithMaxQueueSize(4096),
		sdlog.WithExportMaxBatchSize(c.LogBatchSize),
		sdlog.WithExportInterval(c.LogBatchInterval),
	)
	lp := sdlog.NewLoggerProvider(
		sdlog.WithProcessor(logProcessor),
		sdlog.WithResource(res),
	)

	// ---------- Everything is ready; register the globals ----------
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	// keel.SetupLogger builds its otelslog handler from this global, so it does not need a return value from here.
	otel.SetLoggerProvider(lp)

	shutdown := func() {
		// Bounded so a dead collector cannot hang graceful shutdown.
		shCtx, cancel := sdCtx()
		defer cancel()
		if err := tp.Shutdown(shCtx); err != nil {
			slog.Error("failed to shutdown TracerProvider", "error", err)
		}
		if err := mp.Shutdown(shCtx); err != nil {
			slog.Error("failed to shutdown MeterProvider", "error", err)
		}
		// The LoggerProvider shuts down last, so error logs emitted while tp/mp shut down can still be exported via
		// otelslog.
		if err := lp.Shutdown(shCtx); err != nil {
			slog.Error("failed to shutdown LoggerProvider", "error", err)
		}
	}
	return shutdown, nil
}

// parseConfig takes the otel section and fills in defaults. Whether values are valid (an endpoint when enabled, a
// sample ratio in [0, 1], non-negative intervals and batch sizes) is checked by the rules in conf.proto when the
// config is loaded; this function only handles defaults.
func parseConfig(config *kconfig.Config) *otelConfig {
	var c otelConfig
	if config.Otel == nil || !config.Otel.Enabled {
		return &c
	}

	c.Enabled = true

	c.ServiceName = config.App.GetName()
	c.ServiceVersion = config.App.GetVersion()

	hostname, _ := os.Hostname()
	c.HostName = hostname

	c.Endpoint = config.Otel.Endpoint
	c.Insecure = config.Otel.Insecure

	// proto3 zero-value ambiguity: ratio=0 is treated as "unset" and falls back to 1.0 (sample everything); to stop
	// sampling, turn off otel.enabled instead of setting 0.
	c.TraceSampleRatio = config.Otel.TraceSampleRatio
	if c.TraceSampleRatio == 0 {
		c.TraceSampleRatio = 1.0
	}
	c.MetricInterval = config.Otel.MetricInterval.AsDuration()
	if c.MetricInterval <= 0 {
		c.MetricInterval = 10 * time.Second
	}
	c.LogBatchSize = int(config.Otel.LogBatchSize)
	if c.LogBatchSize <= 0 {
		c.LogBatchSize = 512
	}
	c.LogBatchInterval = config.Otel.LogBatchInterval.AsDuration()
	if c.LogBatchInterval <= 0 {
		c.LogBatchInterval = 1 * time.Second
	}

	return &c
}
