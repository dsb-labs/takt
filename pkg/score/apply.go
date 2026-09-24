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
)

var (
	// ErrMissing is returned when a requirement does not exist on the server.
	ErrMissing = errors.New("missing requirement")
	// ErrUnowned is returned when a resource the score would apply exists
	// without this score's label.
	ErrUnowned = errors.New("resource belongs to something else")
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
	plan, err := NewPlan(rendered)
	if err != nil {
		return Preconditions{}, err
	}

	var preconditions Preconditions
	for _, node := range plan.Requirements {
		_, found, err := lookup(ctx, c, node)
		if err != nil {
			return Preconditions{}, err
		}

		if !found {
			preconditions.Missing = append(preconditions.Missing, node)
		}
	}

	for _, node := range plan.Steps {
		labels, found, err := lookup(ctx, c, node)
		if err != nil {
			return Preconditions{}, err
		}

		if found && labels[LabelName] != rendered.Name {
			preconditions.Unowned = append(preconditions.Unowned, node)
		}
	}

	return preconditions, nil
}

// Apply applies the rendered score in dependency order, checking its
// preconditions first.
//
// Nothing is applied while a requirement is missing or a resource is unowned:
// the error names every one. A failure partway stops at once with no rollback,
// and the report says what landed. The manifests were validated as they were
// rendered, so almost nothing reaches that state.
func Apply(ctx context.Context, c Client, rendered Rendered, options ...ApplyOption) (Report, error) {
	config := new(applyConfig)
	for _, option := range options {
		option(config)
	}

	plan, err := NewPlan(rendered)
	if err != nil {
		return Report{}, err
	}

	preconditions, err := Check(ctx, c, rendered)
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
		if err = apply(ctx, c, rendered, node); err != nil {
			return report, fmt.Errorf("failed to apply %s: %w", node, err)
		}

		report.Applied = append(report.Applied, node)
	}

	return report, nil
}

// apply applies one resource of the rendered score.
func apply(ctx context.Context, c Client, rendered Rendered, node Node) error {
	switch node.Kind {
	case KindVolume:
		index := slices.IndexFunc(rendered.Volumes, func(volume manifest.Volume) bool { return volume.Name == node.Name })
		_, err := c.ApplyVolume(ctx, rendered.Volumes[index])
		return err
	case KindVariable:
		index := slices.IndexFunc(rendered.Variables, func(variable manifest.Variable) bool { return variable.Name == node.Name })
		_, _, err := c.SetVariable(ctx, rendered.Variables[index])
		return err
	case KindWorkload:
		index := slices.IndexFunc(rendered.Workloads, func(spec manifest.Spec) bool { return spec.Name == node.Name })
		_, _, err := c.Apply(ctx, rendered.Workloads[index])
		return err
	case KindService:
		index := slices.IndexFunc(rendered.Services, func(service manifest.Service) bool { return service.Name == node.Name })
		_, err := c.ApplyService(ctx, rendered.Services[index])
		return err
	default:
		return fmt.Errorf("unknown kind %q", node.Kind)
	}
}

// lookup reads a resource, returning its labels and whether it exists.
func lookup(ctx context.Context, c Client, node Node) (map[string]string, bool, error) {
	var (
		labels   map[string]string
		err      error
		notFound error
	)

	switch node.Kind {
	case KindVolume:
		var volume client.Volume
		volume, err = c.GetVolume(ctx, node.Name)
		labels, notFound = volume.Labels, client.ErrVolumeNotFound
	case KindVariable:
		var variable client.Variable
		variable, err = c.GetVariable(ctx, node.Name)
		labels, notFound = variable.Labels, client.ErrVariableNotFound
	case KindSecret:
		var secret client.Secret
		secret, err = c.GetSecret(ctx, node.Name)
		labels, notFound = secret.Labels, client.ErrSecretNotFound
	case KindWorkload:
		var workload client.Workload
		workload, err = c.Get(ctx, node.Name)
		labels, notFound = workload.Spec.Labels, client.ErrWorkloadNotFound
	case KindService:
		var service client.Service
		service, err = c.GetService(ctx, node.Name)
		labels, notFound = service.Labels, client.ErrServiceNotFound
	default:
		return nil, false, fmt.Errorf("unknown kind %q", node.Kind)
	}

	switch {
	case errors.Is(err, notFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("failed to look up %s: %w", node, err)
	default:
		return labels, true, nil
	}
}
