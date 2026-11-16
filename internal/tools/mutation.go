package tools

import (
	"context"
	"fmt"

	"minimalharness/internal/fileguard"
)

// Mutation is prepared before approval and consumed by Dispatch. It binds both
// the requested spelling and resolved target to the same directory handles.
type Mutation struct {
	Path       string
	TargetPath string
	Protected  bool
	executor   *Executor
	target     *fileguard.Target
}

type mutationKey struct{}

func (e *Executor) PrepareMutation(path string) (*Mutation, error) {
	target, err := fileguard.Prepare(e.Root, path)
	if err != nil {
		return nil, err
	}
	return &Mutation{Path: path, TargetPath: target.Path, Protected: protectedSpelling(e.Root, path) || protectedSpelling(e.Root, target.Path) || target.Touches(".harness.json") || target.Touches(".git") || target.Touches(".agents/plugins"), executor: e, target: target}, nil
}

func (m *Mutation) Close() error { return m.target.Close() }

func WithMutation(ctx context.Context, m *Mutation) context.Context {
	return context.WithValue(ctx, mutationKey{}, m)
}

func (e *Executor) mutationTarget(ctx context.Context, path string) (*fileguard.Target, error) {
	m, _ := ctx.Value(mutationKey{}).(*Mutation)
	if m == nil {
		return nil, nil
	}
	if m.executor != e || m.Path != path {
		return nil, fmt.Errorf("mutation does not match approved target")
	}
	return m.target, nil
}
