package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/event"
)

type (
	// The Event type is the service's view of something the server observed about
	// a workload while converging it.
	//
	// It carries both the reason and the sentence rendered from it. A caller doing
	// something with the event matches on the reason, and one showing it to an
	// operator prints the message, so neither has to hold a table of the other.
	Event struct {
		// Why the event was recorded, as a stable code rather than a sentence.
		Reason event.Reason
		// The event as a sentence, rendered when the event is read so the wording is
		// not frozen into what was stored.
		Message string
		// The JSON encoding of the values the message was rendered from.
		Data []byte
		// How many times the event was seen between FirstSeen and LastSeen.
		Count int
		// When the current run of sightings began.
		FirstSeen time.Time
		// When the event was last seen.
		LastSeen time.Time
	}
)

// Events returns what the server recorded about the named workload while
// converging it, most recently seen first, up to limit of them. A non-zero
// since drops the events last seen at or before it.
// Returns ErrWorkloadNotFound when no such workload exists.
//
// The workload is read first so that a name nothing knows is reported as missing
// rather than as having no events, which are different answers to different
// questions.
func (s *WorkloadService) Events(ctx context.Context, name string, since time.Time, limit int) ([]Event, error) {
	if _, err := s.workloads.Get(ctx, name); err != nil {
		if errors.Is(err, database.ErrWorkloadNotFound) {
			return nil, ErrWorkloadNotFound
		}

		return nil, fmt.Errorf("failed to load workload: %w", err)
	}

	if s.events == nil {
		return nil, nil
	}

	rows, err := s.events.List(ctx, name, since, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to read workload events: %w", err)
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, Event{
			Reason:    row.Reason,
			Message:   event.Message(row.Reason, row.Data),
			Data:      row.Data,
			Count:     row.Count,
			FirstSeen: row.FirstSeen,
			LastSeen:  row.LastSeen,
		})
	}

	return events, nil
}

// record stores an event against a workload, saying what an operator asked the
// server to do to it.
//
// The reconciler records what it observed while converging. This records what was
// asked for, which is the other half of the answer to why a workload looks the way
// it does: an operator reading that instances were replaced wants to see the apply
// that caused it on the row above.
//
// A failure here is logged rather than returned. The change itself has already
// landed, so failing the caller's request over an event would report as failed
// something that succeeded.
func (s *WorkloadService) record(ctx context.Context, name string, reason event.Reason, fields event.Fields) {
	record(ctx, s.logger, s.events, name, reason, fields)
}

// record stores an event against the named workload, for the services that hold a
// repository to store it in. Shared because the secret and variable services record
// against a workload too, naming the value of theirs it reads.
//
// An events repository of nil records nothing, so a service wired without one still
// runs.
func record(ctx context.Context, logger *slog.Logger, events WorkloadEventRepository, name string, reason event.Reason, fields event.Fields) {
	if events == nil {
		return
	}

	if err := events.Record(ctx, name, reason, event.Encode(fields)); err != nil {
		logger.With("error", err, "workload", name, "reason", reason).Error("failed to record workload event")
	}
}
