package manifest

type (
	// The Secret type describes what a set stores as a secret: a name, the
	// value it holds, and the labels attached to it.
	//
	// Unlike a workload or a volume, a secret is not described by a file. Its
	// value comes from standard input or a file holding nothing else, so that
	// it never sits in a manifest beside the things that are safe to commit.
	// This is the shape a set takes, not one a parser reads.
	Secret struct {
		// The name that identifies the secret, and which a workload references.
		Name string
		// The value to store, encrypted. Taken exactly as given, including any
		// trailing newline.
		Value []byte
		// Arbitrary key-value pairs attached to the secret. Held to the same
		// rules as a workload's, and as readable as the secret's name.
		Labels map[string]string
	}
)
