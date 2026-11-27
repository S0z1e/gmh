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

// A run lasts through its audit callback, so shutdown cannot release the owning
// writer in the gap between process cleanup and synchronous Bash returning.
func (e *Executor) beginBashRun(ctx context.Context) (context.Context, func(), error) {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if e.jobsClosed {
		return nil, nil, ErrExecutorClosed
	}
	ctx, cancel := context.WithCancelCause(ctx)
	run := &bashRun{cancel: func() { cancel(ErrExecutorClosed) }, done: make(chan struct{})}
	if e.activeBash == nil {
		e.activeBash = make(map[*bashRun]struct{})
	}
	e.activeBash[run] = struct{}{}
	finish := func() {
		cancel(context.Canceled)
		e.jobsMu.Lock()
		delete(e.activeBash, run)
		close(run.done)
		e.jobsMu.Unlock()
	}
	return ctx, finish, nil
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
	var cancelled []*Job
	var closeStops []func() error
	var stops []func()
	var awaited []*Job
	for _, job := range jobs {
		job.mu.Lock()
		if !job.finished && !job.unknown {
			job.stopRequested = true
			if job.closeStop != nil {
				closeStops = append(closeStops, job.closeStop)
			} else if job.closeCancel != nil {
				stops = append(stops, job.closeCancel)
			} else if job.cancel != nil {
				stops = append(stops, job.cancel)
			}
			cancelled = append(cancelled, job)
			awaited = append(awaited, job)
		} else {
			select {
			case <-job.done:
			default:
				// Terminal transition was claimed (e.g. completion audit in flight)
				// but j.done is still open: await completion before returning.
				awaited = append(awaited, job)
			}
		}
		job.mu.Unlock()
	}
	var closeErrs []error
	var stopCh chan error
	if len(closeStops) > 0 {
		stopCh = make(chan error, len(closeStops))
		for _, stop := range closeStops {
			go func(fn func() error) {
				stopCh <- fn()
			}(stop)
		}
	}
	for _, stop := range stops {
		stop()
	}
	for _, run := range runs {
		run.cancel()
	}
	for range closeStops {
		select {
		case err := <-stopCh:
			if err != nil {
				closeErrs = append(closeErrs, err)
			}
		case <-ctx.Done():
			closeErrs = append(closeErrs, ctx.Err())
			return errors.Join(closeErrs...)
		}
	}
	if len(cancelled) > 0 {
		persistDone := make(chan struct{})
		go func() {
			defer close(persistDone)
			for _, job := range cancelled {
				job.persist()
			}
		}()
		select {
		case <-persistDone:
		case <-ctx.Done():
			closeErrs = append(closeErrs, ctx.Err())
			return errors.Join(closeErrs...)
		}
	}
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			closeErrs = append(closeErrs, ctx.Err())
			return errors.Join(closeErrs...)
		}
	}
	for _, job := range awaited {
		if len(closeErrs) > 0 && job.Status() == JobRunning && job.monitorDone != nil {
			// If stop already failed synchronously (WaitError recorded), don't
			// block until ctx.Done() waiting for a job we couldn't stop.
			job.mu.Lock()
			hasWaitErr := job.WaitError != ""
			job.mu.Unlock()
			if hasWaitErr {
				continue
			}
		}
		if job.monitorDone != nil {
			select {
			case <-job.Done():
			case <-job.monitorDone:
				job.mu.Lock()
				failed := !job.finished && !job.unknown && job.WaitError != ""
				waitErr := job.WaitError
				job.mu.Unlock()
				if failed {
					closeErrs = append(closeErrs, errors.New(waitErr))
					continue
				}
				// Even if monitorDone closed after a transient error or race with
				// StopJob, wait for any concurrent/subsequent completion audit and
				// job.Done() before returning.
				select {
				case <-job.Done():
				case <-ctx.Done():
					closeErrs = append(closeErrs, ctx.Err())
					return errors.Join(closeErrs...)
				}
			case <-ctx.Done():
				closeErrs = append(closeErrs, ctx.Err())
				return errors.Join(closeErrs...)
			}
			continue
		}
		select {
		case <-job.Done():
		case <-ctx.Done():
			closeErrs = append(closeErrs, ctx.Err())
			return errors.Join(closeErrs...)
		}
	}
	return errors.Join(closeErrs...)
}
