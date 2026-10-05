package score

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Client interface describes the part of a takt client that applying,
	// deleting and listing scores uses. A *client.Client satisfies it.
	Client interface {
		// ApplyVolume should create or update a volume.
		ApplyVolume(ctx context.Context, volume manifest.Volume, options ...client.ApplyOption) (client.Volume, error)
		// GetVolume should return a volume, or client.ErrVolumeNotFound.
		GetVolume(ctx context.Context, name string) (client.Volume, error)
		// ListVolumes should return the volumes matching every query.
		ListVolumes(ctx context.Context, queries ...string) ([]client.Volume, error)
		// DeleteVolume should delete a volume and the data it holds.
		DeleteVolume(ctx context.Context, name string, options ...client.DeleteVolumeOption) error
		// SetVariable should create or update a variable.
		SetVariable(ctx context.Context, variable manifest.Variable, options ...client.ApplyOption) (client.Variable, bool, error)
		// GetVariable should return a variable, or client.ErrVariableNotFound.
		GetVariable(ctx context.Context, name string) (client.Variable, error)
		// ListVariables should return the variables matching every query.
		ListVariables(ctx context.Context, queries ...string) ([]client.Variable, error)
		// DeleteVariable should delete a variable.
		DeleteVariable(ctx context.Context, name string, options ...client.DeleteVariableOption) error
		// Apply should create or update a workload.
		Apply(ctx context.Context, spec manifest.Spec, options ...client.ApplyOption) (client.Workload, bool, error)
		// Get should return a workload, or client.ErrWorkloadNotFound.
		Get(ctx context.Context, name string) (client.Workload, error)
		// List should return the workloads matching every query.
		List(ctx context.Context, queries ...string) ([]client.Workload, error)
		// Delete should delete a workload, blocking until it is gone when passed
		// client.WithWait.
		Delete(ctx context.Context, name string, options ...client.LifecycleOption) (client.Workload, error)
		// ApplyService should create or update a service.
		ApplyService(ctx context.Context, service manifest.Service, options ...client.ApplyOption) (client.Service, error)
		// GetService should return a service, or client.ErrServiceNotFound.
		GetService(ctx context.Context, name string) (client.Service, error)
		// ListServices should return the services matching every query.
		ListServices(ctx context.Context, queries ...string) ([]client.Service, error)
		// DeleteService should delete a service.
		DeleteService(ctx context.Context, name string) error
		// GetSecret should return a secret's metadata, or client.ErrSecretNotFound.
		GetSecret(ctx context.Context, name string) (client.Secret, error)
	}

	// The Preconditions type reports what stands between a rendered score and a
	// clean apply, all at once, so an operator fixes everything in one pass.
	Preconditions struct {
		// The requirements that do not exist on the server.
		Missing []Node
		// The resources the score would apply that already exist without this
		// score's label. Applying over them would take them from whoever owns
		// them, so they are refused unless the apply adopts them.
		Unowned []Node
	}

	// The Report type records what an apply did.
	Report struct {
		// The resources applied, in the order they were applied. On a failed
		// apply this is what landed before the failure.
		Applied []Node
	}

	// The ApplyOption type modifies how a score is applied.
	ApplyOption func(*applyConfig)

	applyConfig struct {
		adopt bool
	}

	// The resource type is what lookup reads of a resource: its labels, and the
	// tag an apply conditions a write on.
	resource struct {
		labels map[string]string
		etag   string
	}
)

var (
	// ErrMissing is returned when a requirement does not exist on the server.
	ErrMissing = errors.New("missing requirement")
	// ErrUnowned is returned when a resource the score would apply exists
	// without this score's label.
	ErrUnowned = errors.New("resource belongs to something else")
	// ErrChanged is returned when a resource changed on the server between the
	// check and the apply, so what the check judged is not what the apply
	// would have written over.
	ErrChanged = errors.New("resource changed since it was checked")
)

// WithAdopt modifies an apply to take over resources that exist without this
// score's label rather than refusing them. It is how a deployment applied by
// hand becomes a score without deleting it first.
func WithAdopt() ApplyOption {
	return func(c *applyConfig) {
		c.adopt = true
	}
}

// Check reports what has to change before the rendered score can be applied:
// every requirement that does not exist, and every resource that exists without
// this score's label. It contacts the server and changes nothing.
func Check(ctx context.Context, c Client, rendered Rendered) (Preconditions, error) {
	preconditions, _, err := check(ctx, c, rendered)
	return preconditions, err
}

// check is Check, also returning the tag of every step's resource that exists,
// for an apply to condition each write on.
func check(ctx context.Context, c Client, rendered Rendered) (Preconditions, map[Node]string, error) {
	plan, err := NewPlan(rendered)
	if err != nil {
		return Preconditions{}, nil, err
	}

	var preconditions Preconditions
	for _, node := range plan.Requirements {
		_, found, err := lookup(ctx, c, node)
		if err != nil {
			return Preconditions{}, nil, err
		}

		if !found {
			preconditions.Missing = append(preconditions.Missing, node)
		}
	}

	tags := make(map[Node]string)
	for _, node := range plan.Steps {
		existing, found, err := lookup(ctx, c, node)
		if err != nil {
			return Preconditions{}, nil, err
		}

		if !found {
			continue
		}

		tags[node] = existing.etag
		if existing.labels[LabelName] != rendered.Name {
			preconditions.Unowned = append(preconditions.Unowned, node)
		}
	}

	return preconditions, tags, nil
}

// Apply applies the rendered score in dependency order, checking its
// preconditions first.
//
// Nothing is applied while a requirement is missing or a resource is unowned:
// the error names every one. A failure partway stops at once with no rollback,
// and the report says what landed. The manifests were validated as they were
// rendered, so almost nothing reaches that state.
//
// Each write over a resource that existed at the check is conditioned on the tag
// the check read, so a change that lands in between is refused as ErrChanged
// rather than overwritten, and an adoption takes what it was shown. A resource
// absent at the check is created unconditionally, since the server has no way to
// condition a create on absence.
func Apply(ctx context.Context, c Client, rendered Rendered, options ...ApplyOption) (Report, error) {
	config := new(applyConfig)
	for _, option := range options {
		option(config)
	}

	plan, err := NewPlan(rendered)
	if err != nil {
		return Report{}, err
	}

	preconditions, tags, err := check(ctx, c, rendered)
	if err != nil {
		return Report{}, err
	}

	if len(preconditions.Missing) > 0 {
		return Report{}, fmt.Errorf("%w: %s", ErrMissing, joinNodes(preconditions.Missing))
	}

	if len(preconditions.Unowned) > 0 && !config.adopt {
		return Report{}, fmt.Errorf("%w: %s", ErrUnowned, joinNodes(preconditions.Unowned))
	}

	var report Report
	for _, node := range plan.Steps {
		if err = apply(ctx, c, rendered, node, tags[node]); err != nil {
			return report, fmt.Errorf("failed to apply %s: %w", node, err)
		}

		report.Applied = append(report.Applied, node)
	}

	return report, nil
}

// apply applies one resource of the rendered score, conditioned on the tag when
// one was read.
func apply(ctx context.Context, c Client, rendered Rendered, node Node, etag string) error {
	var options []client.ApplyOption
	if etag != "" {
		options = append(options, client.WithIfMatch(etag))
	}

	var (
		err     error
		changed error
	)

	switch node.Kind {
	case KindVolume:
		index := slices.IndexFunc(rendered.Volumes, func(volume manifest.Volume) bool { return volume.Name == node.Name })
		_, err = c.ApplyVolume(ctx, rendered.Volumes[index], options...)
		changed = client.ErrVolumeChanged
	case KindVariable:
		index := slices.IndexFunc(rendered.Variables, func(variable manifest.Variable) bool { return variable.Name == node.Name })
		_, _, err = c.SetVariable(ctx, rendered.Variables[index], options...)
		changed = client.ErrVariableChanged
	case KindWorkload:
		index := slices.IndexFunc(rendered.Workloads, func(spec manifest.Spec) bool { return spec.Name == node.Name })
		_, _, err = c.Apply(ctx, rendered.Workloads[index], options...)
		changed = client.ErrWorkloadChanged
	case KindService:
		index := slices.IndexFunc(rendered.Services, func(service manifest.Service) bool { return service.Name == node.Name })
		_, err = c.ApplyService(ctx, rendered.Services[index], options...)
		changed = client.ErrServiceChanged
	default:
		return fmt.Errorf("unknown kind %q", node.Kind)
	}

	if errors.Is(err, changed) {
		return fmt.Errorf("%w: %v", ErrChanged, err)
	}

	return err
}

// lookup reads a resource, returning it and whether it exists.
func lookup(ctx context.Context, c Client, node Node) (resource, bool, error) {
	var (
		existing resource
		err      error
		notFound error
	)

	switch node.Kind {
	case KindVolume:
		var volume client.Volume
		volume, err = c.GetVolume(ctx, node.Name)
		existing, notFound = resource{volume.Labels, volume.ETag}, client.ErrVolumeNotFound
	case KindVariable:
		var variable client.Variable
		variable, err = c.GetVariable(ctx, node.Name)
		existing, notFound = resource{variable.Labels, variable.ETag}, client.ErrVariableNotFound
	case KindSecret:
		var secret client.Secret
		secret, err = c.GetSecret(ctx, node.Name)
		existing, notFound = resource{secret.Labels, secret.ETag}, client.ErrSecretNotFound
	case KindWorkload:
		var workload client.Workload
		workload, err = c.Get(ctx, node.Name)
		existing, notFound = resource{workload.Spec.Labels, workload.ETag}, client.ErrWorkloadNotFound
	case KindService:
		var service client.Service
		service, err = c.GetService(ctx, node.Name)
		existing, notFound = resource{service.Labels, service.ETag}, client.ErrServiceNotFound
	default:
		return resource{}, false, fmt.Errorf("unknown kind %q", node.Kind)
	}

	switch {
	case errors.Is(err, notFound):
		return resource{}, false, nil
	case err != nil:
		return resource{}, false, fmt.Errorf("failed to look up %s: %w", node, err)
	default:
		return existing, true, nil
	}
}
