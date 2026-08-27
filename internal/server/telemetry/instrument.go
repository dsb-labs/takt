package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// The Outcome type labels a measurement with how the measured operation ended.
// Instruments attach it instead of loose strings, so every package labels the
// same way and a dashboard can group on one attribute.
type Outcome string

const (
	// OutcomeOK labels an operation that succeeded.
	OutcomeOK Outcome = "ok"
	// OutcomeError labels an operation that failed.
	OutcomeError Outcome = "error"
)

// OutcomeOf returns OutcomeError when err is non-nil and OutcomeOK otherwise.
func OutcomeOf(err error) Outcome {
	if err != nil {
		return OutcomeError
	}

	return OutcomeOK
}

// Attribute returns the outcome as an instrument or span attribute.
func (o Outcome) Attribute() attribute.KeyValue {
	return attribute.String("outcome", string(o))
}

// Counter returns the named counter built against meter.
//
// A nil meter, or one that refuses the instrument, yields a counter that records
// nothing — a bad instrument costs its metric rather than the caller. The refusal
// is reported through the OpenTelemetry error handler.
func Counter(meter metric.Meter, name, description, unit string) metric.Int64Counter {
	counter, err := orNoop(meter).Int64Counter(name,
		metric.WithDescription(description),
		metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
		counter, _ = noop.Meter{}.Int64Counter(name)
	}

	return counter
}

// Histogram returns the named histogram built against meter, under the same
// rules as Counter.
func Histogram(meter metric.Meter, name, description, unit string) metric.Float64Histogram {
	histogram, err := orNoop(meter).Float64Histogram(name,
		metric.WithDescription(description),
		metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
		histogram, _ = noop.Meter{}.Float64Histogram(name)
	}

	return histogram
}

// Gauge returns the named gauge built against meter, under the same rules as
// Counter.
func Gauge(meter metric.Meter, name, description, unit string) metric.Int64Gauge {
	gauge, err := orNoop(meter).Int64Gauge(name,
		metric.WithDescription(description),
		metric.WithUnit(unit))
	if err != nil {
		otel.Handle(err)
		gauge, _ = noop.Meter{}.Int64Gauge(name)
	}

	return gauge
}

// Meter returns the named meter from provider, or one that records nothing when the
// provider is nil. A package taking an optional provider calls this once instead of
// guarding every instrument.
//
// The name describes the code the instruments are declared in, so a package asks for
// its own meter under a scope it names itself. Whoever assembles the server has no
// reason to hold a string identifying a package it merely constructs.
func Meter(provider metric.MeterProvider, name string) metric.Meter {
	if provider == nil {
		return noop.Meter{}
	}

	return provider.Meter(name)
}

// Tracer returns the named tracer from provider, or one that records nothing when the
// provider is nil. A package taking an optional provider calls this once instead of
// guarding every span, and names its own scope for the reason Meter does.
func Tracer(provider trace.TracerProvider, name string) trace.Tracer {
	if provider == nil {
		return tracenoop.NewTracerProvider().Tracer(name)
	}

	return provider.Tracer(name)
}

// orNoop returns the given meter, or one that records nothing when it is nil.
func orNoop(meter metric.Meter) metric.Meter {
	if meter == nil {
		return noop.Meter{}
	}

	return meter
}
