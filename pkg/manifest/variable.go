package manifest

type (
	// The Variable type describes what a set stores as a variable: a name, the
	// value it holds, and the labels attached to it.
	//
	// Like a secret, a variable is not described by a file. There is nothing to
	// write down but the value, so this is the shape a set takes rather than
	// one a parser reads.
	Variable struct {
		// The name that identifies the variable, and which a workload references.
		Name string
		// The value to store, as given. An empty string is a value: a workload
		// reading it gets an empty environment variable, which is different
		// from one that is not set.
		Value string
		// Arbitrary key-value pairs attached to the variable. Held to the same
		// rules as a workload's.
		Labels map[string]string
	}
)
