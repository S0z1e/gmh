package tools

import "context"

type invocationKey struct{}
type invocationIdentity struct{ id, callID string }

// WithInvocation binds execution and asynchronous audits to the invocation
// already committed by the agent. It does not change cancellation ownership.
func WithInvocation(ctx context.Context, id, callID string) context.Context {
	return context.WithValue(ctx, invocationKey{}, invocationIdentity{id: id, callID: callID})
}

func invocationFromContext(ctx context.Context) invocationIdentity {
	identity, _ := ctx.Value(invocationKey{}).(invocationIdentity)
	return identity
}
