package tools

import "context"

type invocationKey struct{}
type failureKeyContext struct{}
type invocationIdentity struct{ id, callID string }

// WithInvocation binds execution and asynchronous audits to the invocation
// already committed by the agent. It does not change cancellation ownership.
func WithInvocation(ctx context.Context, id, callID string) context.Context {
	return context.WithValue(ctx, invocationKey{}, invocationIdentity{id: id, callID: callID})
}

// WithFailureKey binds the effective invocation to asynchronous failure
// accounting. It does not grant permission or change cancellation ownership.
func WithFailureKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, failureKeyContext{}, key)
}

func invocationFromContext(ctx context.Context) invocationIdentity {
	identity, _ := ctx.Value(invocationKey{}).(invocationIdentity)
	return identity
}
