package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/port"
	"github.com/dsb-labs/takt/internal/server/spechash"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// Rehash recomputes the named workload's specification hash against the secrets and
// variables it currently reads, reporting whether the hash moved.
//
// This exists for the secret and variable services to call when a value changes. One
// method serves both, because it recomputes against whatever the workload references
// rather than against what it was told changed. Nothing about the specification
// changes: the stored bytes are written back exactly as they were read, and only the
// hash moves. That is what makes a rotated secret or a changed variable read as an
// ordinary specification change, so the reconciler replaces the instances holding the
// old value and the new one is resolved as they start.
//
// Something that has been deleted moves the hash too. The workload is then asking for
// something takt no longer holds, which is reported when it next tries to start
// rather than by silently leaving the old value running.
//
// A pull-always workload's image digest is resolved again here as well, so a rehash
// can also pick up a rebuilt tag — and fails when the registry is unreachable, since
// a hash computed without the digest would claim the image is unchanged when nothing
// checked.
func (s *WorkloadService) Rehash(ctx context.Context, name string) (bool, error) {
	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return false, ErrWorkloadNotFound
	case err != nil:
		return false, fmt.Errorf("failed to load workload: %w", err)
	}

	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return false, err
	}

	read, err := s.resolveReferences(ctx, spec)
	if err != nil {
		return false, err
	}

	digest, err := s.resolveDigest(ctx, spec)
	if err != nil {
		return false, err
	}

	_, hash, err := spechash.Compute(spec, read.hashInputs(digest))
	if err != nil {
		return false, err
	}

	if hash == row.SpecHash {
		// Nothing about the workload moved, which for a workload mounting a value it
		// asked to be signalled about is exactly right: such a value stays out of the
		// hash so that the instance is not replaced. The reconciler is still woken, or
		// nothing would compare what was delivered against what takt now holds until
		// its next tick.
		if len(read.refreshed) > 0 {
			s.wake()
		}

		return false, nil
	}

	// The workload keeps the ports it holds. The specification already names them, so
	// resolving them again would be asking for the allocation takt has, and the write
	// has to carry them or it would clear them.
	held, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return false, fmt.Errorf("failed to read workload ports: %w", err)
	}

	row.SpecHash = hash
	row.Secrets, row.Variables, row.Workloads = read.secrets, read.variables, read.workloads

	if _, _, err = s.workloads.Upsert(ctx, row, 0, held...); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	s.logger.With("workload", name).Debug("workload rehashed")
	s.wake()

	return true, nil
}

// rehashAll rehashes each of the named workloads, which reference the workload whose
// address may have moved, recording the given reason against each one whose hash
// did move.
//
// Separate from redeploy so that a caller which has already read the referencing
// workloads — a deletion, which had to read them to refuse one — does not read them a
// second time to act on them.
//
// The event is recorded after the rehash rather than before it, because only the
// rehash knows whether anything moved. Every apply of a workload rehashes what reads
// it, and most of them leave the address where it was.
func (s *WorkloadService) rehashAll(ctx context.Context, name string, referencing []string, reason event.Reason) {
	for _, workload := range referencing {
		changed, err := s.Rehash(ctx, workload)
		if err != nil {
			s.logger.With("workload", workload, "references", name, "error", err).
				Error("failed to rehash a workload referencing one whose address may have moved")

			continue
		}

		if changed {
			s.record(ctx, workload, reason, event.Fields{Name: name})
		}
	}
}

// redeploy rehashes the workloads referencing the named one, so that a host port that
// moved replaces the instances reading the address it was reached at.
//
// This is the one place a hash moves for a reason nothing went through the service
// for. A secret or a variable changes because somebody set it, where a host port is
// reallocated by the reconciler when a workload fails to start. Without this the
// reference reads as automatic and quietly is not.
//
// A failure is logged rather than returned. The workload whose ports moved is already
// stored, and reporting its apply or its reallocation as failed would describe
// something that did not happen. A consumer left unrehashed is holding an address that
// may well still reach the workload, and the next thing to touch it recomputes the
// hash against what takt currently holds.
func (s *WorkloadService) redeploy(ctx context.Context, name string) {
	referencing, err := s.workloads.ReferencedBy(ctx, name)
	if err != nil {
		s.logger.With("workload", name, "error", err).Error("failed to read the workloads referencing this one")

		return
	}

	s.rehashAll(ctx, name, referencing, event.AddressMoved)
}

// Reallocate gives the named workload fresh host ports for any it holds
// dynamically, reporting whether anything changed.
//
// This exists for the reconciler to call when a workload fails to start, which may
// be because a host port takt chose has been taken by something outside takt. Only
// dynamic ports move: a pinned port was asked for explicitly, so replacing it would
// be overriding the operator rather than revising a guess, and a workload with no
// dynamic ports is left entirely alone.
//
// The workload's stored specification is rewritten with the new ports, which bumps
// its version. That is what makes the reconciler replace the container bound to the
// old port rather than leaving it running on an address nothing records.
func (s *WorkloadService) Reallocate(ctx context.Context, name string) (bool, error) {
	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return false, ErrWorkloadNotFound
	case err != nil:
		return false, fmt.Errorf("failed to load workload: %w", err)
	}

	held, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return false, fmt.Errorf("failed to read workload ports: %w", err)
	}

	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return false, err
	}

	// Dropping the dynamic allocations is what makes resolution pick new ports for
	// them, since resolution only reuses what it finds still held.
	var pinned []port.Claim
	var dynamic bool

	current := heldClaims(held)
	for _, claim := range current {
		if claim.Dynamic {
			dynamic = true
			continue
		}

		pinned = append(pinned, claim)
	}

	if !dynamic {
		return false, nil
	}

	// The stored specification already has its host ports filled in, so the dynamic
	// ones are cleared to ask for a fresh allocation rather than the same port back.
	claims, err := s.claims.Resolve(ctx, name, pinned, port.Requested(spec.Ports, current), spec.Count)
	if err != nil {
		return false, err
	}

	ports := allocations(claims, row.ID)

	// Read again rather than carried over, for the same reason the ports are handed
	// back to the write: the stored links are replaced by whatever the write is given,
	// so a row that named none would have its references forgotten.
	read, err := s.resolveReferences(ctx, spec)
	if err != nil {
		return false, err
	}

	digest, err := s.resolveDigest(ctx, spec)
	if err != nil {
		return false, err
	}

	encoded, hash, err := spechash.Compute(port.Resolved(spec, claims), read.hashInputs(digest))
	if err != nil {
		return false, err
	}

	row.Spec, row.SpecHash = encoded, hash
	row.Secrets, row.Variables, row.Workloads = read.secrets, read.variables, read.workloads

	// The ports travel with the write, as they do for an apply. Claiming them
	// separately beforehand would not survive it: the write replaces a workload's
	// allocation with whatever it was handed, so an Upsert given none clears the
	// rows that were just claimed and leaves the specification naming host ports
	// nothing holds.
	if _, _, err = s.workloads.Upsert(ctx, row, 0, ports...); err != nil {
		return false, fmt.Errorf("failed to store workload: %w", err)
	}

	// The whole point of the reallocation is that the workload's address moved, so
	// everything reading it is now holding one that reaches nothing.
	s.redeploy(ctx, name)

	s.wake()

	return true, nil
}

// ReallocateInstance abandons one instance's dynamic host ports and allocates new
// ones, leaving every other instance's allocation and the stored specification
// alone. Returns false when the instance holds nothing dynamic to move.
//
// The first instance's ports are the workload's advertised address and live in the
// stored specification, so moving them is a specification change and takes the full
// Reallocate path: the hash moves, every instance is replaced, and everything
// reading the address is redeployed. A later instance's ports live only in the port
// rows, so moving them restarts that instance and nothing else.
func (s *WorkloadService) ReallocateInstance(ctx context.Context, name string, instance int) (bool, error) {
	if instance == 0 {
		return s.Reallocate(ctx, name)
	}

	row, err := s.workloads.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrWorkloadNotFound):
		return false, ErrWorkloadNotFound
	case err != nil:
		return false, fmt.Errorf("failed to load workload: %w", err)
	}

	held, err := s.ports.List(ctx, row.ID)
	if err != nil {
		return false, fmt.Errorf("failed to read workload ports: %w", err)
	}

	spec, err := manifest.DecodeWorkload(row.Spec)
	if err != nil {
		return false, err
	}

	// Dropping the instance's dynamic allocations is what makes resolution pick new
	// ports for them. Every other claim is kept, so resolution hands the other
	// instances exactly what they hold.
	var dynamic bool

	current := heldClaims(held)
	kept := make([]port.Claim, 0, len(current))

	for _, claim := range current {
		if claim.Dynamic && claim.Instance == instance {
			dynamic = true

			continue
		}

		kept = append(kept, claim)
	}

	if !dynamic {
		return false, nil
	}

	claims, err := s.claims.Resolve(ctx, name, kept, port.Requested(spec.Ports, current), spec.Count)
	if err != nil {
		return false, err
	}

	// The rows alone. The specification carries only the first instance's ports,
	// none of which moved, so there is no hash to recompute and nothing to
	// redeploy.
	if err = s.ports.Claim(ctx, row.ID, allocations(claims, row.ID)); err != nil {
		return false, fmt.Errorf("failed to claim workload ports: %w", err)
	}

	s.wake()

	return true, nil
}
