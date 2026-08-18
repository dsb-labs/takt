package manifest

type (
	// The Runtime type names the runtime a workload is run by, which determines
	// which of a specification's runtime blocks is used.
	Runtime string

	// The Spec type describes the desired state of a workload.
	//
	// It is the canonical shape of a workload throughout orca's public API: what
	// Parse produces from a manifest file, and what the client submits. Optional
	// values are plain zero values rather than pointers, so callers can build one
	// by hand without ceremony.
	Spec struct {
		// The manifest schema version. Must be "v1".
		Version string
		// The name that identifies the workload.
		Name string
		// The cron expression describing when the workload should run. Accepted
		// and stored, but not yet acted on.
		Schedule string
		// Arbitrary key-value pairs attached to the workload.
		Labels map[string]string
		// The container to run. Exactly one runtime must be set.
		Container *Container
		// The script to run. Exactly one runtime must be set.
		Script *Script
	}

	// The Container type describes the container a workload runs.
	Container struct {
		// The image reference to run.
		Image string
		// Environment variables set inside the container.
		Env map[string]string
		// Port mappings to publish, in "host:container" form.
		Ports []string
	}

	// The Script type describes the script a workload runs. Exactly one of Source
	// and Raw must be set.
	Script struct {
		// The URL the script is fetched from.
		Source string
		// The script body, given inline.
		Raw string
	}
)

const (
	// RuntimeContainer is the runtime that runs a workload as a container.
	RuntimeContainer Runtime = "container"
	// RuntimeScript is the runtime that runs a workload as a script.
	RuntimeScript Runtime = "script"
)
