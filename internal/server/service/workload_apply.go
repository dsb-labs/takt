package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/event"
	"github.com/dsb-labs/takt/internal/server/port"
	"github.com/dsb-labs/takt/internal/server/specdiff"
	"github.com/dsb-labs/takt/internal/server/spechash"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The DryRun type reports what applying a specification would do, having done
	// none of it.
	DryRun struct {
		// The specification as it would be stored: defaults in place, volumes
		// resolved to the paths they live at, and every port settled that could be
		// settled without allocating one.
		Spec manifest.Spec
		// The hash the apply would store. Empty when a host port has yet to be
		// allocated, since the allocation reaches the hash and nothing has chosen
		// one.
		SpecHash string
		// Whether nothing holds the name, so applying creates the workload.
		Created bool
		// Whether applying moves the stored hash, so the reconciler replaces
		// whatever is running. False for a workload that does not exist, which has
		// nothing to replace.
		Replaced bool
		// The paths into the reported specification whose values takt settles only
		// as it applies. A host port it has yet to allocate is the only one, and it
		// is reported rather than invented.
		Unknown []string
		// The paths into the reported specification that differ from the one
		// stored. Empty for a workload that does not exist, which has nothing to
		// differ from.
		//
		// A workload can be replaced with none of these set. The hash covers what
		// the workload reads as well as what it says, so an image rebuilt under the
		// same tag moves the hash with nothing in the specification changing.
		Changed []string
	}
)

// Apply stores spec as the desired state for its name, returning the resulting
// workload and whether it was newly created.
//
// Applying an unchanged specification is a no-op that leaves the version alone;
// a changed one increments it, which is what later causes the reconciler to
// replace any running instance. Returns manifest.ErrNoRuntime when the
// specification names no runtime, or ErrUnsupportedRuntime when it names one the
// server cannot run.
//
// A non-zero ifMatch conditions the apply on the workload still being at that
// version, reporting ErrWorkloadChanged when it is not.
func (s *WorkloadService) Apply(ctx context.Context, spec manifest.Spec, ifMatch int) (Workload, bool, error) {
	resolved, err := s.resolve(ctx, spec)
	if err != nil {
		return Workload{}, false, err
	}

	stored, created, err := s.store(ctx, resolved, ifMatch)
	if err != nil {
		return Workload{}, false, err
	}

	// Whatever reads this workload's address is rehashed after it has landed, since
	// an apply may have moved the ports it publishes.
	s.redeploy(ctx, stored.Name)

	s.logger.With("workload", stored.Name, "version", stored.Version, "created", created).Debug("workload applied")

	// An apply that changed nothing records nothing. The version only moves when the
	// specification did, so this tells a workload that was just applied from one
	// re-applied unchanged by whatever runs the manifests on a loop — and only the
	// first of those explains anything the reconciler goes on to do.
	switch {
	case created:
		s.record(ctx, stored.Name, event.Applied, event.Fields{
			Version: stored.Version,
			Hash:    stored.SpecHash,
		})
	case stored.Version != resolved.existing.Version:
		s.record(ctx, stored.Name, event.SpecificationModified, event.Fields{
			Version:  stored.Version,
			Hash:     stored.SpecHash,
			Previous: resolved.existing.SpecHash,
		})
	}

	s.wake()

	workload, err := s.hydrate(ctx, stored)
	if err != nil {
		return Workload{}, false, err
	}

	return workload, created, nil
}

// DryRun reports what applying spec would do, and writes nothing.
//
// The specification is resolved exactly as an apply resolves it, so everything an
// apply refuses this refuses too: a volume, secret, variable or workload the
// specification names and nothing holds, a pinned host port another workload has,
// a workload mid-teardown, and a specification that is not runnable.
//
// Ports are settled against the allocations the workload already holds and nothing
// else. A mapping that would need one allocated is reported with no host port and
// its path named in Unknown, because allocating is a write: one that claimed a port,
// or moved a sticky allocation, would change the node while reporting that it had
// not.
//
// A pending allocation also leaves the hash empty. The host ports reach the hash, so
// one computed before they are settled would be a hash the apply never stores. Such
// an apply still replaces whatever is running, since an allocation it does not yet
// hold is a specification that changed.
//
// The fields that differ from the stored specification are reported as paths as
// well. They say what about the workload would move, where the hash says only that
// something would.
func (s *WorkloadService) DryRun(ctx context.Context, spec manifest.Spec) (DryRun, error) {
	resolved, err := s.resolve(ctx, spec)
	if err != nil {
		return DryRun{}, err
	}

	claims, pending, err := s.claims.Preview(ctx, resolved.spec.Name, heldClaims(resolved.held), resolved.spec.Ports, resolved.spec.Count)
	if err != nil {
		return DryRun{}, err
	}

	settled := port.Resolved(resolved.spec, claims)

	// The encoding is wanted whether or not the hash is: it is what the stored
	// specification is compared against, and a port yet to be allocated changes
	// nothing about how the rest of the specification encodes.
	encoded, hash, err := spechash.Compute(settled, resolved.read.hashInputs(resolved.digest))
	if err != nil {
		return DryRun{}, fmt.Errorf("failed to hash specification: %w", err)
	}

	run := DryRun{
		Spec:     settled,
		Created:  !resolved.exists,
		Replaced: resolved.exists,
		Unknown:  unknown(settled.Ports),
	}

	if resolved.exists {
		if run.Changed, err = specdiff.Changed(resolved.existing.Spec, encoded, run.Unknown); err != nil {
			return DryRun{}, err
		}
	}

	if pending {
		return run, nil
	}

	run.SpecHash = hash
	run.Replaced = resolved.exists && hash != resolved.existing.SpecHash

	return run, nil
}

// unknown names the ports takt settles only as it applies, as paths into the
// specification it reports.
//
// Paths rather than a flag on each port, because the question is which values are
// not yet known rather than which ports are dynamic. They are written in the syntax
// a list query already uses, so an operator meets one path syntax rather than two.
func unknown(mappings []manifest.Port) []string {
	var paths []string

	for i, mapping := range mappings {
		if mapping.From == 0 {
			paths = append(paths, fmt.Sprintf("$.ports[%d].from", i))
		}
	}

	return paths
}

// store resolves the specification's ports, writes it, and claims the ports it
// settled on.
//
// Allocation reads the ports already promised and then claims one, so two applies
// racing each other can choose the same free port. The unique constraint on the
// claim means one of them loses. That collision is takt's to resolve rather than the
// caller's, so a dynamic port is simply resolved again against what is now allocated.
// A pinned port that collides is a different matter entirely: the caller asked for
// something specific and has to be told it isn't available.
func (s *WorkloadService) store(ctx context.Context, resolved resolution, ifMatch int) (database.Workload, bool, error) {
	// Bounded because a caller waiting on a request would rather hear that takt
	// couldn't settle its ports than wait indefinitely for a quiet moment.
	const attempts = 5

	spec := resolved.spec
	current := heldClaims(resolved.held)

	for attempt := range attempts {
		claims, err := s.claims.Resolve(ctx, spec.Name, current, spec.Ports, spec.Count)
		if err != nil {
			return database.Workload{}, false, err
		}

		ports := allocations(claims, "")

		encoded, hash, err := spechash.Compute(port.Resolved(spec, claims), resolved.read.hashInputs(resolved.digest))
		if err != nil {
			return database.Workload{}, false, err
		}

		row := database.Workload{
			Name:      spec.Name,
			Runtime:   string(resolved.runtime),
			Spec:      encoded,
			SpecHash:  hash,
			Secrets:   resolved.read.secrets,
			Variables: resolved.read.variables,
			Workloads: resolved.read.workloads,
			Labels:    spec.Labels,
		}

		// The row and its ports are written together, so a claim that loses a race
		// leaves no workload behind for the reconciler to start against ports
		// nothing holds.
		stored, created, err := s.workloads.Upsert(ctx, row, ifMatch, ports...)
		switch {
		case err == nil:
			return stored, created, nil
		case errors.Is(err, database.ErrWorkloadDeleting):
			// A delete landed between resolving the specification and writing it.
			return database.Workload{}, false, ErrWorkloadDeleting
		case errors.Is(err, database.ErrWorkloadChanged):
			return database.Workload{}, false, ErrWorkloadChanged
		case errors.Is(err, database.ErrWorkloadNotFound):
			// Only reachable on a conditional apply, which names a version a
			// workload that does not exist cannot be at.
			return database.Workload{}, false, ErrWorkloadChanged
		case !errors.Is(err, database.ErrHostPortTaken):
			return database.Workload{}, false, fmt.Errorf("failed to store workload: %w", err)
		case port.Pinned(spec.Ports):
			return database.Workload{}, false, fmt.Errorf("%w: %v", port.ErrHostPortTaken, err)
		}

		s.logger.With("workload", spec.Name, "attempt", attempt+1).
			Debug("host port was claimed by another workload, allocating again")
	}

	return database.Workload{}, false, fmt.Errorf("%w: gave up after %d attempts", port.ErrHostPortTaken, attempts)
}

// heldClaims maps stored allocations onto what claiming reads. The workload
// identifier is dropped, because settling a workload's ports does not need to know
// which workload it is settling.
func heldClaims(ports []database.Port) []port.Claim {
	claims := make([]port.Claim, 0, len(ports))
	for _, allocation := range ports {
		claims = append(claims, port.Claim{
			Instance:  allocation.Instance,
			Name:      allocation.Name,
			Container: allocation.Container,
			Host:      allocation.Host,
			Protocol:  port.Protocol(allocation.Protocol),
			Dynamic:   allocation.Dynamic,
		})
	}

	return claims
}

// allocations maps settled claims onto the rows that record them, against the
// workload they belong to. The identifier is empty for an apply, whose row is written
// in the same statement and has none to give yet.
func allocations(claims []port.Claim, workloadID string) []database.Port {
	ports := make([]database.Port, 0, len(claims))
	for _, claim := range claims {
		ports = append(ports, database.Port{
			WorkloadID: workloadID,
			Instance:   claim.Instance,
			Name:       claim.Name,
			Container:  claim.Container,
			Host:       claim.Host,
			Protocol:   string(claim.Protocol),
			Dynamic:    claim.Dynamic,
		})
	}

	return ports
}
