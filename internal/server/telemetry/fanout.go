package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"slices"
)

type (
	// The fanout type is an slog handler that dispatches each record to every
	// handler it holds.
	fanout struct {
		handlers []slog.Handler
	}
)

// Fanout returns a handler that dispatches each record to every given handler
// whose Enabled reports true for it. Each handler keeps its own level, so a
// quiet stderr handler does not censor what an exporting handler carries.
func Fanout(handlers ...slog.Handler) slog.Handler {
	return fanout{handlers: handlers}
}

// Enabled reports whether any of the handlers accepts records at the given
// level.
func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	return slices.ContainsFunc(f.handlers, func(handler slog.Handler) bool {
		return handler.Enabled(ctx, level)
	})
}

// Handle dispatches the record to every handler that accepts its level.
func (f fanout) Handle(ctx context.Context, record slog.Record) error {
	errs := make([]error, 0, len(f.handlers))
	for _, handler := range f.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}

		// Cloned because a handler is free to retain what it is given, and the
		// next handler in the loop is given the same record.
		errs = append(errs, handler.Handle(ctx, record.Clone()))
	}

	return errors.Join(errs...)
}

// WithAttrs returns a fanout whose handlers each carry the given attributes.
func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(f.handlers))
	for i, handler := range f.handlers {
		handlers[i] = handler.WithAttrs(slices.Clone(attrs))
	}

	return fanout{handlers: handlers}
}

// WithGroup returns a fanout whose handlers each open the given group.
func (f fanout) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(f.handlers))
	for i, handler := range f.handlers {
		handlers[i] = handler.WithGroup(name)
	}

	return fanout{handlers: handlers}
}
