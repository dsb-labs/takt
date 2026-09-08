// Package telemetry provides the OpenTelemetry providers the server runs with.
// Metrics are always collected and gathered by the /api/v1/system/metrics endpoint. Traces and
// logs are exported over OTLP when an endpoint is configured and are inert
// otherwise.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type (
	// The Telemetry type contains the OpenTelemetry providers the server runs
	// with: a meter provider gathered by the /api/v1/system/metrics endpoint, a tracer
	// provider, and an optional slog handler that carries log records to an
	// exporter.
	Telemetry struct {
		registry  *prometheus.Registry
		meters    *sdkmetric.MeterProvider
		traces    trace.TracerProvider
		handler   slog.Handler
		shutdowns []func(context.Context) error
	}

	// The Config type contains fields used to construct the Telemetry.
	Config struct {
		// The OTLP endpoint traces and logs are exported to, over HTTP, as a
		// URL. The scheme decides whether the connection uses TLS. Empty
		// exports neither signal.
		Endpoint string
		// Replaces the OTLP span exporter when set. Takes precedence over
		// Endpoint. Tests use this to write spans somewhere they can read.
		SpanExporter sdktrace.SpanExporter
		// Replaces the OTLP log exporter when set, under the same rules as
		// SpanExporter.
		LogExporter sdklog.Exporter
	}
)

// New returns a new Telemetry built from the given configuration.
//
// Metrics need no configuration: instruments created from the meter provider are
// always collected and served by the Gatherer. Traces and logs are only exported
// when the configuration names an OTLP endpoint or injects an exporter — without
// either, the tracer provider is a no-op and the log handler is nil.
//
// Everything beyond the endpoint — headers, timeouts, sampling, resource
// attributes — is read from the standard OTEL_* environment variables the SDK
// already honours, rather than repeated in takt's own configuration.
func New(ctx context.Context, config Config) (*Telemetry, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		// The host name is what tells one node's telemetry from another's once
		// several export to the same collector.
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName("takt"),
			semconv.ServiceVersion(version()),
		),
		// Last, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the
		// values above.
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build telemetry resource: %w", err)
	}

	registry := prometheus.NewRegistry()
	reader, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("failed to construct prometheus exporter: %w", err)
	}

	meters := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	// The runtime's own metrics — goroutines, memory, garbage collection — are
	// the failure class nothing measuring workloads can see: the server itself
	// leaking.
	if err = otelruntime.Start(otelruntime.WithMeterProvider(meters)); err != nil {
		return nil, fmt.Errorf("failed to start runtime metrics: %w", err)
	}

	telemetry := &Telemetry{
		registry:  registry,
		meters:    meters,
		traces:    tracenoop.NewTracerProvider(),
		shutdowns: []func(context.Context) error{meters.Shutdown},
	}

	spans, logs, err := exporters(ctx, config)
	if err != nil {
		return nil, err
	}

	if spans != nil {
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(spans),
			sdktrace.WithResource(res),
		)

		telemetry.traces = provider
		telemetry.shutdowns = append(telemetry.shutdowns, provider.Shutdown)
	}

	if logs != nil {
		provider := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logs)),
			sdklog.WithResource(res),
		)

		telemetry.handler = otelslog.NewHandler("takt", otelslog.WithLoggerProvider(provider))
		telemetry.shutdowns = append(telemetry.shutdowns, provider.Shutdown)
	}

	return telemetry, nil
}

// MeterProvider returns the provider instruments are created from. Everything it
// records is served by the Gatherer.
func (t *Telemetry) MeterProvider() metric.MeterProvider {
	return t.meters
}

// TracerProvider returns the provider spans are created from. It is a no-op
// provider unless the configuration asked for trace export.
func (t *Telemetry) TracerProvider() trace.TracerProvider {
	return t.traces
}

// Gatherer returns the prometheus gatherer holding everything the meter provider
// records. The /api/v1/system/metrics endpoint reads from it.
func (t *Telemetry) Gatherer() prometheus.Gatherer {
	return t.registry
}

// LogHandler returns a handler that carries log records to the configured
// exporter. It is nil when the configuration asked for no log export, which the
// caller treats as "log to stderr alone".
func (t *Telemetry) LogHandler() slog.Handler {
	return t.handler
}

// Shutdown flushes and stops every provider. Exported signals are batched, so a
// server that exits without calling this loses whatever the batches still hold.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	errs := make([]error, 0, len(t.shutdowns))
	for _, shutdown := range t.shutdowns {
		errs = append(errs, shutdown(ctx))
	}

	return errors.Join(errs...)
}

// exporters returns the span and log exporters the configuration asks for. Either
// is nil when nothing asked for its signal.
func exporters(ctx context.Context, config Config) (sdktrace.SpanExporter, sdklog.Exporter, error) {
	spans := config.SpanExporter
	logs := config.LogExporter
	if config.Endpoint == "" || (spans != nil && logs != nil) {
		return spans, logs, nil
	}

	endpoint, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse otlp endpoint: %w", err)
	}

	// The exporters take a host and a flag rather than a URL, so the scheme
	// becomes the flag here. Everything finer-grained is theirs to read from the
	// standard environment variables.
	insecure := endpoint.Scheme == "http"

	if spans == nil {
		options := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint.Host)}
		if insecure {
			options = append(options, otlptracehttp.WithInsecure())
		}

		spans, err = otlptracehttp.New(ctx, options...)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to construct otlp trace exporter: %w", err)
		}
	}

	if logs == nil {
		options := []otlploghttp.Option{otlploghttp.WithEndpoint(endpoint.Host)}
		if insecure {
			options = append(options, otlploghttp.WithInsecure())
		}

		logs, err = otlploghttp.New(ctx, options...)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to construct otlp log exporter: %w", err)
		}
	}

	return spans, logs, nil
}

// version reports the version of the running binary, matching what the CLI
// reports as its own.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	return info.Main.Version
}
