package loadtest

import (
	"fmt"

	"github.com/dsb-labs/orca/pkg/manifest"
)

const (
	// The image every container workload runs. Trivial and already on any host that
	// has run the end-to-end suite, so a run measures orca rather than a pull.
	image = "busybox:latest"
	// What a workload does while it is up, which is as little as possible.
	idle = "while true; do echo tick; sleep 5; done"
	// What a workload that is meant to fail does, which is fail immediately. This
	// is what puts the restart backoff and the decision to give up under load.
	fail = "exit 1"
	// What a scheduled workload does, which is finish.
	once = "echo ran"
	// The port a workload listens on inside its runtime, and serves a health check
	// from. Every workload publishing a port uses the same one, because the host
	// port is what has to differ and orca is what chooses it.
	port = 8080
)

type (
	// The Workload type is one member of a fleet: the specification to apply, and
	// whether it was built to fail.
	Workload struct {
		// The specification to apply.
		Spec manifest.Spec
		// Whether the workload is meant to fail, and so is left out of the check
		// that the fleet converged. A workload built to exit non-zero never reaches
		// running, so counting it as unconverged would report the scenario working
		// as the scenario failing.
		Fails bool
	}

	// The Names type holds what a run created, so that churn can pick from them and
	// teardown can remove them.
	Names struct {
		// The workloads, in the order they were built.
		Workloads []string
		// The secrets the fleet reads.
		Secrets []string
		// The variables the fleet reads.
		Variables []string
		// The volumes the fleet mounts.
		Volumes []string
	}
)

// Build returns the fleet a scenario describes, along with the names of everything it
// reads.
//
// Features are spread deterministically rather than at random: a workload carries a
// feature when its index falls inside that feature's share of the fleet. Two runs of
// one scenario therefore apply the same fleet, which is what makes their numbers
// comparable — a random spread would vary the work between runs and put that variance
// into the measurement.
func Build(scenario Scenario, prefix string) ([]Workload, Names) {
	names := Names{
		Secrets:   nameSet(prefix, "secret", scenario.Resources.Secrets),
		Variables: nameSet(prefix, "variable", scenario.Resources.Variables),
		Volumes:   nameSet(prefix, "volume", scenario.Resources.Volumes),
	}

	total := scenario.Workloads()
	workloads := make([]Workload, 0, total)

	for i := range scenario.Fleet.Containers {
		name := fmt.Sprintf("%s-container-%04d", prefix, i)
		names.Workloads = append(names.Workloads, name)
		workloads = append(workloads, build(scenario, names, name, i, total, true))
	}

	for i := range scenario.Fleet.Exec {
		name := fmt.Sprintf("%s-exec-%04d", prefix, i)
		names.Workloads = append(names.Workloads, name)

		// Offset by the containers already built so that a feature's share is spread
		// across the whole fleet rather than restarting for each runtime. Without it
		// a scenario asking for a tenth of anything would put all of it in the
		// containers and none in the exec workloads.
		workloads = append(workloads, build(scenario, names, name, scenario.Fleet.Containers+i, total, false))
	}

	return workloads, names
}

func build(scenario Scenario, names Names, name string, index, total int, container bool) Workload {
	fleet := scenario.Fleet

	spec := manifest.Spec{
		Version: "v1",
		Name:    name,
		Labels:  map[string]string{"loadtest": scenario.Name},
		Restart: &manifest.Restart{Policy: manifest.RestartAlways},
		Env:     map[string]string{},
	}

	workload := Workload{Fails: has(index, total, fleet.Failing)}
	scheduled := !workload.Fails && has(index, total, fleet.Scheduled)

	command := idle
	switch {
	case workload.Fails:
		command = fail
	case scheduled:
		command = once

		// A schedule replaces the restart policy: a job that runs on a schedule is
		// not one that is kept up, and orca refuses the combination.
		spec.Schedule = &manifest.Schedule{Cron: "* * * * *"}
		spec.Restart = &manifest.Restart{Policy: manifest.RestartNever}
	}

	if container {
		spec.Container = &manifest.Container{Image: image, Command: []string{"sh", "-c", command}}
	} else {
		spec.Exec = &manifest.Exec{Command: []string{"sh", "-c", command}}
	}

	// A workload publishes at most one port, so the two shares are laid end to end
	// rather than tested separately. The scenario refuses a pair that overlaps.
	switch {
	case has(index, total, fleet.DynamicPorts):
		spec.Ports = []manifest.Port{{Name: "http", To: port}}
	case within(index, total, fleet.DynamicPorts, fleet.DynamicPorts+fleet.FixedPorts):
		// Pinned above the range orca allocates from, so a scenario's fixed ports
		// collide with each other rather than with what the allocator hands out.
		spec.Ports = []manifest.Port{{Name: "http", To: port, From: 40000 + index}}
	}

	// Only a workload that publishes something can be checked, since a check reaches
	// a port, and a scheduled workload cannot be checked at all — it is not up
	// between runs. The manifest refuses both, so a fleet that built them would be
	// one the server would not accept.
	if len(spec.Ports) > 0 && !scheduled && has(index, total, fleet.HealthChecks) {
		spec.Health = &manifest.Health{TCP: true, Port: "http"}
	}

	if len(names.Secrets) > 0 && has(index, total, fleet.ReadsSecret) {
		spec.Env["SECRET"] = "${secret:" + pick(names.Secrets, index) + "}"
	}

	if len(names.Variables) > 0 && has(index, total, fleet.ReadsVariable) {
		spec.Env["VARIABLE"] = "${var:" + pick(names.Variables, index) + "}"
	}

	if len(names.Secrets) > 0 && has(index, total, fleet.MountsSecret) {
		spec.Volumes = append(spec.Volumes, manifest.VolumeMount{
			Secret: pick(names.Secrets, index), To: "/var/loadtest-secret",
		})
	}

	if len(names.Variables) > 0 && has(index, total, fleet.MountsVariable) {
		spec.Volumes = append(spec.Volumes, manifest.VolumeMount{
			Var: pick(names.Variables, index), To: "/var/loadtest-variable",
		})
	}

	// Volumes reach container workloads only. An exec workload runs on the host, so
	// a volume mount is a bind the runtime has nowhere to perform.
	if container && len(names.Volumes) > 0 && has(index, total, fleet.MountsVolume) {
		spec.Volumes = append(spec.Volumes, manifest.VolumeMount{
			Name: pick(names.Volumes, index), To: "/var/loadtest-volume",
		})
	}

	spec.Defaults()
	workload.Spec = spec

	return workload
}

// has reports whether the workload at index carries a feature held by the given
// proportion of a fleet of the given size.
func has(index, total int, proportion float64) bool {
	return within(index, total, 0, proportion)
}

// within reports whether the workload at index falls inside a share of the fleet
// running from one proportion to another.
//
// The share is taken from the front of the fleet, so the workloads carrying a feature
// are the same on every run of a scenario.
func within(index, total int, from, to float64) bool {
	if to <= from || total == 0 {
		return false
	}

	return index >= int(from*float64(total)) && index < int(to*float64(total))
}

// pick returns one of the names, chosen by index so that a fleet spreads itself
// across everything a scenario created rather than crowding onto the first.
func pick(names []string, index int) string {
	return names[index%len(names)]
}

func nameSet(prefix, kind string, count int) []string {
	names := make([]string, 0, count)
	for i := range count {
		names = append(names, fmt.Sprintf("%s-%s-%04d", prefix, kind, i))
	}

	return names
}
