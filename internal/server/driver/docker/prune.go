package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/dsb-labs/takt/pkg/manifest"
)

// Prune removes the images that no workload in keep names and no container uses,
// once they have gone unreferenced for the driver's delay. Nothing else on the
// daemon is touched: not a container, a volume, a network or the build cache.
//
// An image is held by any container that references it, whether or not takt
// created it and whether or not it is running. That is what bounds what a prune
// can reach: a container started by hand or by a compose stack beside takt holds
// its image, and so does the container the driver retains for an instance's
// output. Docker refuses to remove an image a container holds without being
// forced, and the driver never forces, so the daemon's own rule is the backstop
// behind the driver's.
//
// A workload names its image in any state — running, suspended, waiting on a
// schedule or held in backoff — since a suspended workload resumes and a job
// recurs, and each would pull its image again if the prune had taken it. A
// specification naming a runtime other than this one holds nothing.
//
// The delay is measured from when the driver first saw the image unreferenced
// rather than from when the image was built or pulled: an old image pulled a
// moment ago is not one that has gone unused.
func (d *Driver) Prune(ctx context.Context, keep []manifest.Spec) error {
	if !d.prune {
		return nil
	}

	ctx, span := d.tracer.Start(ctx, "image.prune")
	defer span.End()

	named := make(map[string]struct{})
	for _, spec := range keep {
		if spec.Container == nil {
			continue
		}

		named[normalise(spec.Container.Image)] = struct{}{}
	}

	images, err := d.client.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	// Every container rather than takt's own, since a container anyone created
	// holds its image.
	containers, err := d.client.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	used := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		used[c.ImageID] = struct{}{}
	}

	span.SetAttributes(
		attribute.Int("takt.images", len(images)),
		attribute.Int("takt.containers", len(containers)),
	)

	var failed []error
	for _, img := range d.unreferencedFor(images, named, used) {
		if err := d.removeImage(ctx, img); err != nil {
			failed = append(failed, err)
		}
	}

	return errors.Join(failed...)
}

// unreferencedFor returns the images nothing references that have stayed that way
// for the delay, recording when each was first seen unreferenced and forgetting
// the ones that are referenced again or gone.
func (d *Driver) unreferencedFor(images []image.Summary, named, used map[string]struct{}) []image.Summary {
	now := d.now()

	d.unreferencedMux.Lock()
	defer d.unreferencedMux.Unlock()

	listed := make(map[string]struct{}, len(images))

	var due []image.Summary
	for _, img := range images {
		if referenced(img, named, used) {
			delete(d.unreferenced, img.ID)

			continue
		}

		listed[img.ID] = struct{}{}

		since, ok := d.unreferenced[img.ID]
		if !ok {
			since = now
			d.unreferenced[img.ID] = since
		}

		if now.Sub(since) >= d.pruneDelay {
			due = append(due, img)
		}
	}

	maps.DeleteFunc(d.unreferenced, func(id string, _ time.Time) bool {
		_, ok := listed[id]

		return !ok
	})

	return due
}

// referenced reports whether a workload names the image under any of its tags or
// digests, or whether a container uses it.
func referenced(img image.Summary, named, used map[string]struct{}) bool {
	if _, ok := used[img.ID]; ok {
		return true
	}

	return slices.ContainsFunc(slices.Concat(img.RepoTags, img.RepoDigests), func(ref string) bool {
		_, ok := named[normalise(ref)]

		return ok
	})
}

// normalise returns the given image reference in its full form, so that the
// several ways of writing one image compare as one. A reference that does not
// parse — an older daemon lists a dangling image's tag as "<none>:<none>" — is
// returned as written, which nothing a manifest names can equal.
func normalise(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}

	return reference.TagNameOnly(named).String()
}

// removeImage removes one image the driver decided nothing references. A tagged
// image is removed tag by tag, since the daemon refuses to remove by identifier
// an image tagged in more than one repository and the driver does not force it.
// The last tag removed takes the image with it. An image with no tags is removed
// by its identifier, which is the only name it has.
func (d *Driver) removeImage(ctx context.Context, img image.Summary) error {
	ctx, span := d.tracer.Start(ctx, "image.remove",
		trace.WithAttributes(attribute.String("takt.image", img.ID)))
	defer span.End()

	names := img.RepoTags
	if len(names) == 0 {
		names = []string{img.ID}
	}

	deleted := false
	for _, name := range names {
		responses, err := d.client.ImageRemove(ctx, name, client.ImageRemoveOptions{PruneChildren: true})
		if err != nil {
			span.RecordError(err)

			return fmt.Errorf("failed to remove image %s: %w", name, err)
		}

		deleted = deleted || slices.ContainsFunc(responses, func(response image.DeleteResponse) bool {
			return response.Deleted == img.ID
		})
	}

	if !deleted {
		return nil
	}

	d.unreferencedMux.Lock()
	delete(d.unreferenced, img.ID)
	d.unreferencedMux.Unlock()

	d.instruments.pruned.Add(ctx, 1)
	d.instruments.reclaimed.Add(ctx, img.Size)

	d.logger.With("image", img.ID, "tags", img.RepoTags, "size", img.Size).Info("removed unreferenced image")

	return nil
}
