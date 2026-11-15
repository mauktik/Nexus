package provider

import "context"

// Background is a provider with work that runs inside nexusd between
// requests while it is enabled by configuration, such as a sampler. The
// daemon calls RunBackground in a goroutine of its own when the provider
// becomes enabled (at startup, after READY=1, or by a reload) and cancels
// ctx when a reload disables it or nexusd stops; it waits a bounded time for
// RunBackground to return. RunBackground must return promptly once ctx is
// done, recover its own panics per unit of work, and release what it holds
// when it returns. It is never called for a disabled provider, by
// validate-config, or by CheckConfig. A run that overran its stop bound may
// still be returning when a new run starts: the newest run owns the
// provider's state.
type Background interface {
	RunBackground(ctx context.Context)
}
