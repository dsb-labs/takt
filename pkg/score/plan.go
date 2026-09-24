package score

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Kind type names what kind of resource a node in a plan is.
	Kind string

	// The Node type identifies one resource a plan applies or requires.
	Node struct {
		// What kind of resource the node is.
		Kind Kind
		// The name of the resource.
		Name string
	}

	// The Plan type is the order a score's resources are applied in, and what
	// has to exist before they are.
	//
	// Delete and prune walk the steps backwards. Waiting for each step is what
	// makes that legal: a mounted volume and a read variable are both refused
	// while a workload holds them, and teardown is asynchronous.
	Plan struct {
		// The resources the score applies, in an order where everything a
		// resource depends on comes before it.
		Steps []Node
		// The resources the score's workloads depend on but the score does not
		// apply, which must exist before it is applied. Every declared secret
		// and required variable is here, with any volume or workload a
		// manifest names that the score does not.
		Requirements []Node
	}

	// The inventory type is what a plan is built from: the names of every
	// resource in a set, with the workload specifications the dependencies are
	// read from. A rendered score is one, and so is what a label query lists.
	inventory struct {
		volumes   []string
		workloads []manifest.Spec
		services  []string
		variables []string
	}
)

const (
	// KindVolume is a volume.
	KindVolume Kind = "volume"
	// KindVariable is a variable.
	KindVariable Kind = "variable"
	// KindSecret is a secret, which a score never applies.
	KindSecret Kind = "secret"
	// KindWorkload is a workload.
	KindWorkload Kind = "workload"
	// KindService is a service.
	KindService Kind = "service"
)

var (
	// ErrCycle is returned when the workloads in a score reference each other
	// in a loop, so no order applies them.
	ErrCycle = errors.New("dependency cycle")
	// ErrUndeclared is returned when a workload reads a variable or a secret the
	// score neither sets nor declares.
	ErrUndeclared = errors.New("undeclared dependency")
)

// The order kinds are applied in when nothing else decides, which is what
// puts every volume before every workload and every service after.
var kindOrder = map[Kind]int{KindVolume: 0, KindVariable: 1, KindSecret: 2, KindWorkload: 3, KindService: 4}

// NewPlan builds the plan for a rendered score.
//
// A workload depends on the volumes it mounts, the variables and secrets it
// reads, and any workload it reaches through a workload reference, since the
// server refuses a reference to a workload that does not exist yet. Services
// depend on nothing: a target's workloads need not exist.
//
// A variable or secret a workload reads that the score neither sets nor declares
// is refused with ErrUndeclared, so the score stays an honest inventory of what
// its workloads need.
func NewPlan(rendered Rendered) (Plan, error) {
	if err := checkDeclared(rendered); err != nil {
		return Plan{}, err
	}

	held := inventory{
		volumes:   make([]string, 0, len(rendered.Volumes)),
		workloads: rendered.Workloads,
		services:  make([]string, 0, len(rendered.Services)),
		variables: make([]string, 0, len(rendered.Variables)),
	}

	for _, volume := range rendered.Volumes {
		held.volumes = append(held.volumes, volume.Name)
	}

	for _, service := range rendered.Services {
		held.services = append(held.services, service.Name)
	}

	for _, variable := range rendered.Variables {
		held.variables = append(held.variables, variable.Name)
	}

	plan, err := held.plan()
	if err != nil {
		return Plan{}, err
	}

	// Every required variable and declared secret is a requirement whether or
	// not a workload reads it: the declaration is the promise that it exists.
	for _, name := range rendered.Required {
		plan.require(Node{Kind: KindVariable, Name: name})
	}

	for _, name := range rendered.Secrets {
		plan.require(Node{Kind: KindSecret, Name: name})
	}

	slices.SortFunc(plan.Requirements, compareNodes)

	return plan, nil
}

// require adds a node to the requirements unless it is there already.
func (p *Plan) require(node Node) {
	if !slices.Contains(p.Requirements, node) {
		p.Requirements = append(p.Requirements, node)
	}
}

// checkDeclared reports a workload reading a variable or secret the score does
// not list.
func checkDeclared(rendered Rendered) error {
	variables := slices.Clone(rendered.Required)
	for _, variable := range rendered.Variables {
		variables = append(variables, variable.Name)
	}

	for _, workload := range rendered.Workloads {
		references, err := manifest.References(workload)
		if err != nil {
			return fmt.Errorf("failed to read references of workload %s: %w", workload.Name, err)
		}

		for _, name := range manifest.Names(references, manifest.KindVariable) {
			if !slices.Contains(variables, name) {
				return fmt.Errorf("%w: workload %s reads variable %s, which the score does not set or require", ErrUndeclared, workload.Name, name)
			}
		}

		for _, name := range manifest.Names(references, manifest.KindSecret) {
			if !slices.Contains(rendered.Secrets, name) {
				return fmt.Errorf("%w: workload %s reads secret %s, which the score does not declare", ErrUndeclared, workload.Name, name)
			}
		}
	}

	return nil
}

// plan orders the inventory so that everything a resource depends on comes
// before it, and reports what it depends on that it does not hold.
//
// Kahn's algorithm, taking the smallest available node by kind and then name
// at each step, so that the order is stable and kinds group together where
// the dependencies allow.
func (inv inventory) plan() (Plan, error) {
	nodes := make([]Node, 0, len(inv.volumes)+len(inv.workloads)+len(inv.services)+len(inv.variables))
	for _, name := range inv.volumes {
		nodes = append(nodes, Node{Kind: KindVolume, Name: name})
	}

	for _, name := range inv.variables {
		nodes = append(nodes, Node{Kind: KindVariable, Name: name})
	}

	for _, workload := range inv.workloads {
		nodes = append(nodes, Node{Kind: KindWorkload, Name: workload.Name})
	}

	for _, name := range inv.services {
		nodes = append(nodes, Node{Kind: KindService, Name: name})
	}

	held := make(map[Node]struct{}, len(nodes))
	for _, node := range nodes {
		held[node] = struct{}{}
	}

	// What each node depends on, and what depends on it.
	dependencies := make(map[Node][]Node, len(inv.workloads))
	dependents := make(map[Node][]Node, len(nodes))

	var requirements []Node
	for _, workload := range inv.workloads {
		reader := Node{Kind: KindWorkload, Name: workload.Name}
		for _, dependency := range dependenciesOf(workload) {
			if _, ok := held[dependency]; !ok {
				if !slices.Contains(requirements, dependency) {
					requirements = append(requirements, dependency)
				}

				continue
			}

			dependencies[reader] = append(dependencies[reader], dependency)
			dependents[dependency] = append(dependents[dependency], reader)
		}
	}

	remaining := make(map[Node]int, len(nodes))
	for node, deps := range dependencies {
		remaining[node] = len(deps)
	}

	available := slices.DeleteFunc(slices.Clone(nodes), func(node Node) bool { return remaining[node] > 0 })
	slices.SortFunc(available, compareNodes)

	steps := make([]Node, 0, len(nodes))
	for len(available) > 0 {
		next := available[0]
		available = available[1:]
		steps = append(steps, next)

		for _, dependent := range dependents[next] {
			remaining[dependent]--
			if remaining[dependent] == 0 {
				index, _ := slices.BinarySearchFunc(available, dependent, compareNodes)
				available = slices.Insert(available, index, dependent)
			}
		}
	}

	if len(steps) < len(nodes) {
		stuck := slices.DeleteFunc(slices.Clone(nodes), func(node Node) bool { return remaining[node] == 0 })
		slices.SortFunc(stuck, compareNodes)

		return Plan{}, fmt.Errorf("%w: %s", ErrCycle, joinNodes(stuck))
	}

	slices.SortFunc(requirements, compareNodes)

	return Plan{Steps: steps, Requirements: requirements}, nil
}

// dependenciesOf returns what a workload depends on: the volumes it mounts by
// name, the variables and secrets it reads, and the workloads it reaches.
//
// A token reference is not a dependency. Nothing has to exist before a
// workload asserts a principal, and a host path is the host's to provide.
func dependenciesOf(workload manifest.Spec) []Node {
	var nodes []Node

	// References already validated by Render, so the error cannot occur here.
	references, _ := manifest.References(workload)
	for _, reference := range references {
		var node Node
		switch reference.Kind {
		case manifest.KindVariable:
			node = Node{Kind: KindVariable, Name: reference.Name}
		case manifest.KindSecret:
			node = Node{Kind: KindSecret, Name: reference.Name}
		case manifest.KindWorkload:
			node = Node{Kind: KindWorkload, Name: reference.Name}
		default:
			continue
		}

		if !slices.Contains(nodes, node) {
			nodes = append(nodes, node)
		}
	}

	for _, mount := range workload.Volumes {
		if mount.Name == "" {
			continue
		}

		node := Node{Kind: KindVolume, Name: mount.Name}
		if !slices.Contains(nodes, node) {
			nodes = append(nodes, node)
		}
	}

	return nodes
}

// compareNodes orders nodes by the kind order and then by name.
func compareNodes(a, b Node) int {
	return cmp.Or(cmp.Compare(kindOrder[a.Kind], kindOrder[b.Kind]), cmp.Compare(a.Name, b.Name))
}

// joinNodes names several nodes for an error.
func joinNodes(nodes []Node) string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.String())
	}

	return strings.Join(names, ", ")
}

// String returns the node as "kind name".
func (n Node) String() string {
	return string(n.Kind) + " " + n.Name
}
