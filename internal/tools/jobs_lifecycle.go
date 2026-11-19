package tools

import (
	"context"
	"errors"
)

var ErrExecutorClosed = errors.New("bash executor is closed")

type bashRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// SessionJobs returns an existing owner's jobs without selecting that session.
func (e *Executor) SessionJobs(id string) []*Job {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	registry := e.sessionJobs[id]
	if id == e.sessionID {
		registry = e.jobs
	}
	if registry == nil {
		return nil
	}
	return registry.List()
}

// CloseJobs prevents new Bash runs, cancels all current and retained session
// commands, and waits within ctx for process cleanup and job results. Retry is
// safe after a deadline. It does not acknowledge delivery or discard results.
func (e *Executor) CloseJobs(ctx context.Context) error {
	e.jobsMu.Lock()
	e.jobsClosed = true
	registries := map[*JobRegistry]bool{}
	if e.jobs != nil {
		registries[e.jobs] = true
	}
	for _, r := range e.sessionJobs {
		if r != nil {
			registries[r] = true
		}
	}
	var jobs []*Job
	for r := range registries {
		jobs = append(jobs, r.List()...)
	}
	var runs []*bashRun
	for run := range e.activeBash {
		runs = append(runs, run)
	}
	e.jobsMu.Unlock()
	for _, job := range jobs {
		if job.Status() == JobRunning {
			job.Cancel()
		}
	}
	for _, run := range runs {
		run.cancel()
	}
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, job := range jobs {
		select {
		case <-job.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
