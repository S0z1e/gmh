package tools

import (
	"context"
	"sync"
)

// File mutations still serialize with snapshots. A canceled read can leave the
// wait without acquiring the lock or needing a goroutine to acquire it later.
type editMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *editMutex) Lock() { _ = m.LockContext(context.Background()) }
func (m *editMutex) LockContext(ctx context.Context) error {
	m.once.Do(func() { m.token = make(chan struct{}, 1); m.token <- struct{}{} })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	}
}
func (m *editMutex) Unlock() {
	select {
	case m.token <- struct{}{}:
	default:
		panic("unlock of unlocked edit mutex")
	}
}
