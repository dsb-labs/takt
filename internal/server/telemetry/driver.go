package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsb-labs/takt/internal/server/driver"
)

// The name driver spans are created under, which describes the code declaring
// them rather than the driver they happen to wrap.
const driverScope = "github.com/dsb-labs/takt/internal/server/telemetry"

type (
	// The Driver interface describes the runtime operations WrapDriver traces.
	// It mirrors the reconciler's own driver interface, so a wrapped driver can
	// be handed to it directly.
	Driver interface {
		// Name should return the name the driver is known by.
		Name() string
		// Start should run the given workload, returning the driver's handle for it.
		Start(ctx context.Context, w driver.Workload) (string, error)
		// Stop should stop everything the driver runs for a workload.
		Stop(ctx context.Context, id, workload string) error
		// Discard should stop everything the driver runs for a workload and
		// remove all of it.
		Discard(ctx context.Context, id, workload string) error
		// StopInstance should stop what the driver runs for one instance of a
		// workload.
		StopInstance(ctx context.Context, id, workload string, instance int) error
		// DiscardInstance should stop what the driver runs for one instance of a
		// workload and remove all of it.
		DiscardInstance(ctx context.Context, id, workload string, instance int) error
		// Signal should send the named signal to everything the driver runs for
		// a workload.
		Signal(ctx context.Context, id, workload, signal string) error
		// Observe should report every instance the driver is currently running.
		Observe(ctx context.Context) ([]driver.Instance, error)
		// Watch should report changes to the driver's instances.
		Watch(ctx context.Context) (<-chan driver.Event, error)
	}

	// The tracedDriver type is the wrapper WrapDriver returns.
	tracedDriver struct {
		next   Driver
		tracer trace.Tracer
	}
)

// WrapDriver returns the given driver with every operation traced: each call
// becomes a span named for the operation, carrying the driver and the workload
// it was made for, with an error recorded on the span when the call fails.
//
// The spans exist so that every driver explains a converge the same way. The
// docker driver's requests surface through its instrumented HTTP client, but
// those name docker's API rather than takt's intent, and a driver that works
// through syscalls produces nothing at all — a converge with no children is a
// duration with no explanation.
//
// Watch is passed through untraced: it hands over a channel rather than doing
// the work, and a span measuring it would measure nothing. A nil provider
// yields spans that record nothing, following the package's other constructors.
func WrapDriver(d Driver, provider trace.TracerProvider) Driver {
	return &tracedDriver{next: d, tracer: Tracer(provider, driverScope)}
}

// span starts a span for one call against the wrapped driver.
func (t *tracedDriver) span(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, name, trace.WithAttributes(append(attributes,
		attribute.String("takt.driver", t.next.Name()),
	)...))
}

// end finishes a span, recording the error when the call it covered failed.
func end(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	span.End()
}

// Name reports the wrapped driver's name.
func (t *tracedDriver) Name() string {
	return t.next.Name()
}

// Start runs the given workload on the wrapped driver.
func (t *tracedDriver) Start(ctx context.Context, w driver.Workload) (string, error) {
	ctx, span := t.span(ctx, "driver.start",
		attribute.String("takt.workload", w.Name),
		attribute.Int("takt.instance", w.Instance))

	id, err := t.next.Start(ctx, w)
	end(span, err)

	return id, err
}

// Stop stops everything the wrapped driver runs for a workload.
func (t *tracedDriver) Stop(ctx context.Context, id, workload string) error {
	ctx, span := t.span(ctx, "driver.stop",
		attribute.String("takt.workload", workload))

	err := t.next.Stop(ctx, id, workload)
	end(span, err)

	return err
}

// Discard stops everything the wrapped driver runs for a workload and removes
// all of it.
func (t *tracedDriver) Discard(ctx context.Context, id, workload string) error {
	ctx, span := t.span(ctx, "driver.discard",
		attribute.String("takt.workload", workload))

	err := t.next.Discard(ctx, id, workload)
	end(span, err)

	return err
}

// StopInstance stops what the wrapped driver runs for one instance of a
// workload.
func (t *tracedDriver) StopInstance(ctx context.Context, id, workload string, instance int) error {
	ctx, span := t.span(ctx, "driver.stop",
		attribute.String("takt.workload", workload),
		attribute.Int("takt.instance", instance))

	err := t.next.StopInstance(ctx, id, workload, instance)
	end(span, err)

	return err
}

// DiscardInstance stops what the wrapped driver runs for one instance of a
// workload and removes all of it.
func (t *tracedDriver) DiscardInstance(ctx context.Context, id, workload string, instance int) error {
	ctx, span := t.span(ctx, "driver.discard",
		attribute.String("takt.workload", workload),
		attribute.Int("takt.instance", instance))

	err := t.next.DiscardInstance(ctx, id, workload, instance)
	end(span, err)

	return err
}

// Signal sends the named signal to everything the wrapped driver runs for a
// workload.
func (t *tracedDriver) Signal(ctx context.Context, id, workload, signal string) error {
	ctx, span := t.span(ctx, "driver.signal",
		attribute.String("takt.workload", workload),
		attribute.String("takt.signal", signal))

	err := t.next.Signal(ctx, id, workload, signal)
	end(span, err)

	return err
}

// Observe reports every instance the wrapped driver is currently running.
func (t *tracedDriver) Observe(ctx context.Context) ([]driver.Instance, error) {
	ctx, span := t.span(ctx, "driver.observe")

	instances, err := t.next.Observe(ctx)
	end(span, err)

	return instances, err
}

// Watch reports changes to the wrapped driver's instances.
func (t *tracedDriver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	return t.next.Watch(ctx)
}
