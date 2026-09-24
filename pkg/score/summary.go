package score

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dsb-labs/takt/pkg/manifest"
)

// The Summary type reports what a rendered score contains, by name: what show
// prints, and what an operator reads to learn what a score needs before it is
// applied.
type Summary struct {
	// The name the score is installed as.
	Name string
	// The name of the package the score describes.
	Package string
	// The release of the score.
	Release string
	// The names of the volumes the score applies.
	Volumes []string
	// The names of the workloads the score applies.
	Workloads []string
	// The names of the services the score applies.
	Services []string
	// The names of the variables the score sets.
	Variables []string
	// The names of the variables the score requires but does not set.
	Required []string
	// The names of the secrets the score declares, which must be set before
	// it is applied.
	Secrets []string
	// The principals the score's workloads assert through token references,
	// which the access policy has to grant for them to reach the server.
	Principals []string
}

// Summarise returns what the rendered score contains, by name.
func Summarise(rendered Rendered) Summary {
	summary := Summary{
		Name:       rendered.Name,
		Package:    rendered.Package,
		Release:    rendered.Release,
		Volumes:    make([]string, 0, len(rendered.Volumes)),
		Workloads:  make([]string, 0, len(rendered.Workloads)),
		Services:   make([]string, 0, len(rendered.Services)),
		Variables:  make([]string, 0, len(rendered.Variables)),
		Required:   slices.Clone(rendered.Required),
		Secrets:    slices.Clone(rendered.Secrets),
		Principals: Principals(rendered.Workloads),
	}

	for _, volume := range rendered.Volumes {
		summary.Volumes = append(summary.Volumes, volume.Name)
	}

	for _, workload := range rendered.Workloads {
		summary.Workloads = append(summary.Workloads, workload.Name)
	}

	for _, service := range rendered.Services {
		summary.Services = append(summary.Services, service.Name)
	}

	for _, variable := range rendered.Variables {
		summary.Variables = append(summary.Variables, variable.Name)
	}

	return summary
}

// Principals returns every principal the workloads assert through a token
// reference, sorted and without repeats.
//
// A token needs nothing to exist before a workload asserts it, so this is not a
// precondition. It is what an operator reads to know what the policy must grant.
func Principals(workloads []manifest.Spec) []string {
	principals := make([]string, 0)
	for _, workload := range workloads {
		// Render has already validated every workload, so the references parse.
		references, _ := manifest.References(workload)
		for _, name := range manifest.Names(references, manifest.KindToken) {
			if !slices.Contains(principals, name) {
				principals = append(principals, name)
			}
		}
	}

	slices.Sort(principals)

	return principals
}

// Numbered returns the document's text with a line number in front of every
// line, which is how a render failure is printed so the error's line number
// can be found in it.
func (d Document) Numbered() string {
	lines := strings.Split(strings.TrimSuffix(d.Text, "\n"), "\n")

	var out strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&out, "%4d  %s\n", i+1, line)
	}

	return out.String()
}
