package client

import "context"

// The key a Client is stored under in a context. Unexported, so nothing
// outside this package can collide with it.
type contextKey struct{}

// NewContext returns a context that carries the client.
//
// This is how the CLI hands one client, built once from the root command's
// flags, to whichever subcommand runs.
func NewContext(ctx context.Context, c *Client) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// FromContext returns the client the context carries, or nil when it carries
// none.
func FromContext(ctx context.Context) *Client {
	c, _ := ctx.Value(contextKey{}).(*Client)

	return c
}
