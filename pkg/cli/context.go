package cli

import "context"

// The key a Config is stored under in a context. Unexported, so nothing
// outside this package can collide with it.
type contextKey struct{}

// NewContext returns a context that carries the resolved settings.
//
// This is how the CLI hands the settings it resolved once, at the root
// command, to a subcommand that needs more than the client built from them.
// The login command is the reason: it connects without the stored credential
// and writes the settings back beside the token it mints, neither of which
// the shared client can do for it.
func NewContext(ctx context.Context, config Config) context.Context {
	return context.WithValue(ctx, contextKey{}, config)
}

// FromContext returns the settings the context carries, or the zero Config
// when it carries none.
func FromContext(ctx context.Context) Config {
	config, _ := ctx.Value(contextKey{}).(Config)

	return config
}
