package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dsb-labs/takt/internal/server/database"
	"github.com/dsb-labs/takt/internal/server/driver"
	"github.com/dsb-labs/takt/internal/server/resolve"
	"github.com/dsb-labs/takt/internal/server/spechash"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The references type carries what a workload reads and what those things
	// currently hold, from the one scan of a specification to the write that records
	// it and the hash that covers it.
	//
	// The names travel alongside what was read because the two answer different
	// questions. The names are stored, so that finding the workloads to redeploy when
	// something moves does not depend on parsing every specification. What was read
	// reaches the hash and is then discarded.
	references struct {
		// The names of the secrets the specification references.
		secrets []string
		// The names of the variables the specification references.
		variables []string
		// The names of the other workloads the specification references, without
		// repeats: one workload referenced at two ports is one name.
		workloads []string
		// The revision of each secret that exists, keyed by name.
		revisions map[string]string
		// The value of each variable that exists, keyed by name.
		values map[string]string
		// The address each workload reference resolved to, keyed by the reference as
		// it was written. Nil when the specification references no workload, so that
		// one which references none hashes as it did before it could.
		//
		// The whole address rather than the port alone. A workload is started with
		// what this holds, so a host that changed leaves every consumer holding an
		// address that no longer reaches anything — which is a change to the
		// specification in every sense that matters.
		addresses map[string]string
		// The workload references naming a workload that does not exist, and those
		// naming a port the referenced workload does not publish.
		//
		// Recorded rather than returned as an error, because a workload that has gone
		// has to move the hash of whatever reads it rather than stop it being
		// rehashed. Refusing the reference is the business of the caller that can act
		// on it, which is the apply.
		unknown     []string
		absentPorts []string
		// What the specification reads only through a mount naming a signal, which is
		// deliberately kept out of the hash.
		//
		// Such a workload asked to be signalled rather than replaced, and a hash that
		// moved with the value would have the reconciler replace it — which is the one
		// thing naming a signal asks not to happen. That the workload reads it is
		// still recorded, so deleting one still reports the workloads holding it.
		refreshed []manifest.Reference
	}

	// The resolution type carries what resolving a specification established, from
	// the one pass that reads everything it names to the caller that acts on it.
	//
	// The ports are the workload's existing allocations rather than the ones it will
	// be reached at. Settling those allocates, and what resolving establishes is
	// exactly what can be established without writing.
	resolution struct {
		// The specification, with its defaults in place and its volumes resolved to
		// the paths they live at.
		spec manifest.Spec
		// Which runtime the specification names.
		runtime manifest.Runtime
		// The workload as it currently stands, meaningful only when it exists.
		existing database.Workload
		// Whether a workload of this name is already stored.
		exists bool
		// The port allocations the workload already holds. Empty for one that is
		// not stored yet.
		held []database.Port
		// What the specification reads, and what each of those currently holds.
		read references
		// The digest the image resolves to, empty for a workload whose pull policy
		// is not always.
		digest string
	}
)

// resolve establishes everything an apply depends on, and writes nothing.
//
// It sits on its own because two callers need it. An apply resolves and then
// stores. A dry run resolves and then reports what storing would do. A dry run
// resolving a specification its own way would drift from the apply and report
// confidently wrong answers, which is worse than having no dry run.
//
// Ports are the one thing left unsettled here, because settling them allocates and
// allocating writes. Each caller does that for itself.
func (s *WorkloadService) resolve(ctx context.Context, spec manifest.Spec) (resolution, error) {
	// Resolved here rather than trusted from the caller, so that what gets stored and
	// hashed is the specification with its defaults in place however it arrived. A
	// caller that already resolved them changes nothing by asking again.
	spec.Defaults()

	// The runtime is resolved first so that naming none or naming two is reported as
	// exactly that, rather than as a general validation failure.
	runtime, err := manifest.RuntimeOf(spec)
	if err != nil {
		return resolution{}, err
	}

	// Everything else is validated here rather than only in the client that parsed a
	// manifest. The rules are what make a workload runnable at all — a name the
	// runtime can represent, an image to run, a schedule that parses — so a caller
	// that skips the CLI has to be held to them too. Without this the server accepted
	// an unknown schema version, a name breaking its own documented rules, and an
	// empty image that could only ever fail to start.
	if err = manifest.ValidateWorkload(spec); err != nil {
		return resolution{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	// Validation proved the limits are well formed, but whether they can be
	// enforced is a fact about this host that the manifest rules cannot know: the
	// exec driver needs a delegated cgroup subtree. Refused here rather than at
	// start, because the operator applying the manifest is the one who can act on
	// the refusal — a workload accepted and never started would fail where nobody
	// is looking. A dry run shares this resolve, so it reports the same refusal.
	if spec.Resources != nil {
		if d, ok := s.drivers[string(runtime)]; ok {
			if err = d.Enforceable(); err != nil {
				return resolution{}, fmt.Errorf("%w: %v", ErrUnsupportedRuntime, err)
			}
		}
	}

	// A workload mid-teardown cannot be resurrected by re-applying it: the
	// reconciler is still removing its work, so accepting the change would race
	// that teardown and could leave the new instance being torn down instead.
	existing, err := s.workloads.Get(ctx, spec.Name)
	switch {
	case err != nil && !errors.Is(err, database.ErrWorkloadNotFound):
		return resolution{}, fmt.Errorf("failed to load workload: %w", err)
	case err == nil && !existing.DeletedAt.IsZero():
		return resolution{}, ErrWorkloadDeleting
	}

	resolved := resolution{runtime: runtime, existing: existing, exists: err == nil}

	// The ports the workload already holds are read here so that settling them keeps
	// the allocations it has. They are part of what gets hashed, so a reallocation
	// reads as an ordinary specification change and replaces the container bound to
	// the old port.
	if resolved.exists {
		if resolved.held, err = s.ports.List(ctx, existing.ID); err != nil {
			return resolution{}, fmt.Errorf("failed to read workload ports: %w", err)
		}
	}

	// Volumes are resolved to the paths they live at before the specification is
	// hashed, for the same reason ports are: the runtime is then handed a path rather
	// than a name to look up, and a volume whose path changed reads as an ordinary
	// change and replaces the instances bound to the old one.
	//
	// Resolved here rather than while storing, since unlike a port allocation there
	// is no race to lose: a volume either exists or it does not.
	if resolved.spec, err = s.resolveVolumes(ctx, spec); err != nil {
		return resolution{}, err
	}

	// The secrets the workload reads are resolved to their revisions rather than their
	// values, and the variables to their values. Either reaches the hash so that
	// changing one replaces the instances reading it. A secret's value stays out of
	// both the hash and the stored specification, and is read only as the workload
	// starts.
	//
	// A reference to something that does not exist is refused here rather than at
	// start. The workload could never run, and the operator asking for it is the one
	// who can fix the name.
	read, err := s.resolveReferences(ctx, resolved.spec)
	if err != nil {
		return resolution{}, err
	}

	if absent := missing(read.secrets, read.revisions); len(absent) > 0 {
		return resolution{}, fmt.Errorf("%w: %s", ErrSecretNotFound, strings.Join(absent, ", "))
	}

	if absent := missing(read.variables, read.values); len(absent) > 0 {
		return resolution{}, fmt.Errorf("%w: %s", ErrVariableNotFound, strings.Join(absent, ", "))
	}

	// A workload that does not exist is refused here rather than at start, as a secret
	// is. The workload could never run, and the operator asking for it is the one who
	// can fix the name or apply the workload it names first.
	if len(read.unknown) > 0 {
		return resolution{}, fmt.Errorf("%w: %s", ErrWorkloadNotFound, strings.Join(read.unknown, ", "))
	}

	if len(read.absentPorts) > 0 {
		return resolution{}, fmt.Errorf("%w: %s", ErrPortNotPublished, strings.Join(read.absentPorts, ", "))
	}

	resolved.read = read

	// Resolved here rather than while storing, whose port retries would repeat the
	// registry round-trip for nothing: the digest does not depend on the ports.
	if resolved.digest, err = s.resolveDigest(ctx, resolved.spec); err != nil {
		return resolution{}, err
	}

	return resolved, nil
}

// resolveVolumes fills in where each mounted volume lives on the host, rejecting a
// specification naming one that does not exist and a host path this server's
// configuration does not allow.
//
// A volume has to exist before it can be mounted. Creating one here would make a
// mistyped name a second empty volume, which reads as success while the data the
// workload wanted sits under the name that was meant.
// Only a mount naming a volume is resolved. A mounted secret or variable is written
// by the reconciler as the workload starts, at a path that changes with every version,
// so storing one would move the hash for a reason the operator did not ask for and put
// takt's own layout in the API.
//
// A path mount is gated here rather than by validation, because whether a host
// allows a path is this host's configuration rather than a property of the
// manifest. The gate runs when a specification is accepted: one already stored
// keeps running if the list later narrows, the way an exec workload keeps the
// paths it was started with.
func (s *WorkloadService) resolveVolumes(ctx context.Context, spec manifest.Spec) (manifest.Spec, error) {
	if len(spec.Volumes) == 0 {
		return spec, nil
	}

	mounts := make([]manifest.VolumeMount, 0, len(spec.Volumes))
	for _, mount := range spec.Volumes {
		// Which source a mount names is read through the same rules validation
		// applied, rather than by inspecting the fields again here.
		kind, err := manifest.KindOf(mount)
		if err != nil {
			return spec, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
		}

		switch kind {
		case manifest.MountPath:
			// The path is stored as written. It is resolved again as each instance
			// starts, since the tree can change in between, and the driver is handed
			// what that resolution reaches.
			if _, err := driver.ResolveHostPath(mount.Path, s.hostPaths); err != nil {
				return spec, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
			}

			mounts = append(mounts, mount)

			continue
		case manifest.MountVolume:
		default:
			// The source fields are carried across untouched, so what the workload
			// reads is stored as written rather than as a value.
			mounts = append(mounts, mount)

			continue
		}

		if s.volumes == nil {
			return spec, fmt.Errorf("%w: this server holds no volumes", ErrVolumeNotFound)
		}

		// Whatever went wrong is returned as it stands, including a volume that does
		// not exist: the locator already names what it could not find, so wrapping it
		// again would only repeat the name.
		path, err := s.volumes.Path(ctx, mount.Name)
		if err != nil {
			return spec, err
		}

		mount.From = path
		mounts = append(mounts, mount)
	}

	// The specification is taken by value, so this replaces only this copy's slice
	// header and leaves the caller's alone.
	spec.Volumes = mounts

	return spec, nil
}

// resolveReferences returns what the specification's environment references, along
// with what each of those currently holds.
//
// The specification comes back untouched, unlike ports and volumes: a reference is
// stored as written, because resolving a secret would put its value in the database
// and a variable is stored the same way for consistency. What was read travels
// separately, reaching the hash without being stored.
//
// Something that does not exist is simply absent rather than an error. That is what
// makes deleting a secret, a variable or a referenced workload move the hash of the
// workloads reading it, which is how they come to report that something they need has
// gone. Refusing the reference is the business of the caller that can act on it.
func (s *WorkloadService) resolveReferences(ctx context.Context, spec manifest.Spec) (references, error) {
	found, err := manifest.References(spec)
	if err != nil {
		return references{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	refreshed, err := manifest.Refreshed(spec)
	if err != nil {
		return references{}, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	resolved := references{
		secrets:   manifest.Names(found, manifest.KindSecret),
		variables: manifest.Names(found, manifest.KindVariable),
		workloads: manifest.Names(found, manifest.KindWorkload),
		refreshed: refreshed,
	}

	switch {
	case len(resolved.secrets) == 0:
	case s.secrets == nil:
		return references{}, fmt.Errorf("%w: this server holds no secrets", ErrSecretNotFound)
	default:
		if resolved.revisions, err = s.secrets.Revisions(ctx, resolved.secrets); err != nil {
			return references{}, fmt.Errorf("failed to read secret revisions: %w", err)
		}
	}

	switch {
	case len(resolved.variables) == 0:
	case s.variables == nil:
		return references{}, fmt.Errorf("%w: this server holds no variables", ErrVariableNotFound)
	default:
		if resolved.values, err = s.variables.Values(ctx, resolved.variables); err != nil {
			return references{}, fmt.Errorf("failed to read variable values: %w", err)
		}
	}

	// The port a reference names is part of what is being resolved, so these are read
	// one reference at a time rather than one workload at a time.
	for _, reference := range manifest.Of(found, manifest.KindWorkload) {
		if s.addresses == nil {
			return references{}, fmt.Errorf("%w: this server resolves no workload addresses", ErrWorkloadNotFound)
		}

		// The first instance's resolution stands in for the workload here: what is
		// being established is that the reference resolves at all, and the stored
		// hash carries that instance's view.
		address, err := s.addresses.Address(ctx, reference, spec.Name, 0)
		switch {
		case errors.Is(err, database.ErrWorkloadNotFound):
			resolved.unknown = append(resolved.unknown, reference.String())

			continue
		case errors.Is(err, resolve.ErrPortNotPublished):
			resolved.absentPorts = append(resolved.absentPorts, reference.String())

			continue
		case err != nil:
			return references{}, fmt.Errorf("failed to resolve the address of workload %s: %w", reference.Name, err)
		}

		if resolved.addresses == nil {
			resolved.addresses = make(map[string]string, len(found))
		}

		resolved.addresses[reference.String()] = address
	}

	return resolved, nil
}

// resolveDigest returns the digest a pull-always workload's image currently
// resolves to, and the empty string for every other workload.
//
// The digest is read whenever a hash is computed — an apply, a rehash, a port
// reallocation — rather than stored, so each of those picks up a rebuilt tag and
// none can disagree with the others about what the tag holds. The cost is that
// each is a registry round-trip, and each fails when the registry is unreachable.
// That is the honest outcome: a hash computed without the digest would claim the
// image is unchanged when nothing checked.
func (s *WorkloadService) resolveDigest(ctx context.Context, spec manifest.Spec) (string, error) {
	if spec.Container == nil || spec.Container.Pull != manifest.PullAlways {
		return "", nil
	}

	if s.images == nil {
		return "", fmt.Errorf("%w: this server cannot resolve image digests", ErrInvalidSpec)
	}

	digest, err := s.images.Digest(ctx, spec.Container.Image)
	if err != nil {
		return "", fmt.Errorf("failed to resolve image digest: %w", err)
	}

	return digest, nil
}

// missing names the referenced things of one kind that do not exist.
//
// Every name is checked rather than the counts compared, so that an operator is told
// which one to create rather than that one of them is absent.
func missing(names []string, held map[string]string) []string {
	var absent []string
	for _, name := range names {
		if _, ok := held[name]; !ok {
			absent = append(absent, name)
		}
	}

	return absent
}

// hashInputs returns what the specification's hash covers beyond the specification
// itself.
//
// Narrower than the references type it comes from. The names a workload reads are
// stored so that finding what to redeploy does not depend on parsing every
// specification, and a reference naming something that has gone is the apply's
// business. Neither reaches the hash.
func (r references) hashInputs(digest string) spechash.Inputs {
	return spechash.Inputs{
		Revisions: r.revisions,
		Values:    r.values,
		Addresses: r.addresses,
		Refreshed: r.refreshed,
		Digest:    digest,
	}
}
