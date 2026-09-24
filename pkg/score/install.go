package score

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/dsb-labs/takt/pkg/client"
)

// The Install type is what one install of a score holds on the server, found
// by its label rather than by rendering anything: an install can be inspected
// and removed without the directory or the values that produced it.
type Install struct {
	// The name the score was installed as.
	Name string
	// The releases the install's resources carry, sorted. More than one means
	// an apply failed partway and the next one has not finished the job.
	Releases []string
	// The names of the volumes carrying the install's label.
	Volumes []string
	// The names of the workloads carrying the install's label.
	Workloads []string
	// The names of the services carrying the install's label.
	Services []string
	// The names of the variables carrying the install's label.
	Variables []string
}

// List returns every install on the server, grouped by the install label and
// sorted by name.
//
// The label query the server offers is an equality, so this lists every
// resource and keeps the ones carrying the label.
func List(ctx context.Context, c Client) ([]Install, error) {
	return collect(ctx, c)
}

// Installed returns the install with the given name, or one holding nothing
// when no resource carries its label.
func Installed(ctx context.Context, c Client, name string) (Install, error) {
	installs, err := collect(ctx, c, "$.labels."+LabelName+"="+name)
	if err != nil {
		return Install{}, err
	}

	if len(installs) == 0 {
		return Install{Name: name}, nil
	}

	return installs[0], nil
}

// collect lists every kind of resource matching the queries and groups those
// carrying the install label by it.
func collect(ctx context.Context, c Client, queries ...string) ([]Install, error) {
	installs := make(map[string]*Install)
	install := func(labels map[string]string) (*Install, bool) {
		name, ok := labels[LabelName]
		if !ok {
			return nil, false
		}

		if _, ok := installs[name]; !ok {
			installs[name] = &Install{Name: name}
		}

		found := installs[name]
		if release := labels[LabelRelease]; !slices.Contains(found.Releases, release) {
			found.Releases = append(found.Releases, release)
		}

		return found, true
	}

	volumes, err := c.ListVolumes(ctx, queries...)
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w", err)
	}

	for _, volume := range volumes {
		if found, ok := install(volume.Labels); ok {
			found.Volumes = append(found.Volumes, volume.Name)
		}
	}

	variables, err := c.ListVariables(ctx, queries...)
	if err != nil {
		return nil, fmt.Errorf("failed to list variables: %w", err)
	}

	for _, variable := range variables {
		if found, ok := install(variable.Labels); ok {
			found.Variables = append(found.Variables, variable.Name)
		}
	}

	workloads, err := c.List(ctx, queries...)
	if err != nil {
		return nil, fmt.Errorf("failed to list workloads: %w", err)
	}

	for _, workload := range workloads {
		if found, ok := install(workload.Spec.Labels); ok {
			found.Workloads = append(found.Workloads, workload.Name)
		}
	}

	services, err := c.ListServices(ctx, queries...)
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	for _, service := range services {
		if found, ok := install(service.Labels); ok {
			found.Services = append(found.Services, service.Name)
		}
	}

	out := make([]Install, 0, len(installs))
	for _, found := range installs {
		slices.Sort(found.Releases)
		slices.Sort(found.Volumes)
		slices.Sort(found.Variables)
		slices.Sort(found.Workloads)
		slices.Sort(found.Services)
		out = append(out, *found)
	}

	slices.SortFunc(out, func(a, b Install) int { return strings.Compare(a.Name, b.Name) })

	return out, nil
}

// Nodes returns every resource of the install as a node.
func (i Install) Nodes() []Node {
	nodes := make([]Node, 0, len(i.Volumes)+len(i.Variables)+len(i.Workloads)+len(i.Services))
	for _, name := range i.Volumes {
		nodes = append(nodes, Node{Kind: KindVolume, Name: name})
	}

	for _, name := range i.Variables {
		nodes = append(nodes, Node{Kind: KindVariable, Name: name})
	}

	for _, name := range i.Workloads {
		nodes = append(nodes, Node{Kind: KindWorkload, Name: name})
	}

	for _, name := range i.Services {
		nodes = append(nodes, Node{Kind: KindService, Name: name})
	}

	return nodes
}

// Prunable returns the resources of the install that the rendered score no
// longer names: what an apply with pruning removes once it has applied.
func Prunable(rendered Rendered, install Install) []Node {
	keep := make(map[Node]struct{})
	for _, volume := range rendered.Volumes {
		keep[Node{Kind: KindVolume, Name: volume.Name}] = struct{}{}
	}

	for _, variable := range rendered.Variables {
		keep[Node{Kind: KindVariable, Name: variable.Name}] = struct{}{}
	}

	for _, workload := range rendered.Workloads {
		keep[Node{Kind: KindWorkload, Name: workload.Name}] = struct{}{}
	}

	for _, service := range rendered.Services {
		keep[Node{Kind: KindService, Name: service.Name}] = struct{}{}
	}

	return slices.DeleteFunc(install.Nodes(), func(node Node) bool {
		_, ok := keep[node]
		return ok
	})
}

// Destroy deletes the given resources in reverse dependency order, waiting for
// each workload to be torn down before moving on, and returns what it deleted.
//
// Waiting is what makes the order legal. A volume a workload mounts and a
// variable a workload reads are both refused while the workload holds them,
// and teardown is asynchronous. Volumes are deleted with the data they hold,
// so a caller should confirm before calling this. A failure stops at once and
// the result says what was deleted before it.
func Destroy(ctx context.Context, c Client, nodes []Node) ([]Node, error) {
	steps, err := order(ctx, c, nodes)
	if err != nil {
		return nil, err
	}

	slices.Reverse(steps)

	var deleted []Node
	for _, node := range steps {
		if err = destroy(ctx, c, node); err != nil {
			return deleted, fmt.Errorf("failed to delete %s: %w", node, err)
		}

		deleted = append(deleted, node)
	}

	return deleted, nil
}

// order returns the nodes in apply order, reading each workload's
// specification to learn what depends on what.
func order(ctx context.Context, c Client, nodes []Node) ([]Node, error) {
	var held inventory
	for _, node := range nodes {
		switch node.Kind {
		case KindVolume:
			held.volumes = append(held.volumes, node.Name)
		case KindVariable:
			held.variables = append(held.variables, node.Name)
		case KindService:
			held.services = append(held.services, node.Name)
		case KindWorkload:
			workload, err := c.Get(ctx, node.Name)
			if err != nil {
				return nil, fmt.Errorf("failed to get workload %s: %w", node.Name, err)
			}

			held.workloads = append(held.workloads, workload.Spec)
		default:
			return nil, fmt.Errorf("unknown kind %q", node.Kind)
		}
	}

	plan, err := held.plan()
	if err != nil {
		return nil, err
	}

	return plan.Steps, nil
}

// destroy deletes one resource, waiting for a workload to be gone.
func destroy(ctx context.Context, c Client, node Node) error {
	switch node.Kind {
	case KindVolume:
		return c.DeleteVolume(ctx, node.Name)
	case KindVariable:
		return c.DeleteVariable(ctx, node.Name)
	case KindWorkload:
		_, err := c.Delete(ctx, node.Name, client.WithWait())
		return err
	case KindService:
		return c.DeleteService(ctx, node.Name)
	default:
		return fmt.Errorf("unknown kind %q", node.Kind)
	}
}
