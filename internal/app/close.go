package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"minimalharness/internal/agent"
	"minimalharness/internal/session"
)

func (a *App) retainOrClose(store *session.Store) {
	if store == nil {
		return
	}
	if a.Cfg != nil && a.Cfg.Exec != nil {
		for _, job := range a.Cfg.Exec.SessionJobs(store.ID) {
			if !job.Delivered() {
				if a.retainedStores == nil {
					a.retainedStores = make(map[*session.Store]struct{})
				}
				a.retainedStores[store] = struct{}{}
				return
			}
		}
	}
	_ = store.Close()
}

// Close stops commands and saves finished results to their original sessions
// before releasing writers. Call after the active Runner has stopped. Failed
// saves retain their writers and undelivered results for a later retry.
func (a *App) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.CloseContext(ctx)
}

func (a *App) CloseContext(ctx context.Context) error {
	var cleanupErr error
	if a.Cfg != nil && a.Cfg.Exec != nil {
		cleanupErr = a.Cfg.Exec.CloseJobs(ctx)
	}
	closeErr := cleanupErr
	stores := map[*session.Store]bool{}
	if a.Store != nil {
		stores[a.Store] = true
	}
	for store := range a.retainedStores {
		stores[store] = true
	}
	for store := range stores {
		unfinished := false
		if a.Cfg != nil && a.Cfg.Exec != nil {
			jobs := a.Cfg.Exec.SessionJobs(store.ID)
			if err := agent.SaveJobResults(store, jobs); err != nil {
				closeErr = errors.Join(closeErr, fmt.Errorf("save session %s jobs: %w", store.ID, err))
				continue
			}
			for _, job := range jobs {
				if !job.Delivered() {
					unfinished = true
				}
			}
		}
		// Raw synchronous runs have no session registry entry. Until
		// cleanup succeeds, conservatively retain all writer leases.
		if unfinished || cleanupErr != nil {
			continue
		}
		closeErr = errors.Join(closeErr, store.Close())
		delete(a.retainedStores, store)
	}
	return closeErr
}
