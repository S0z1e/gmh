package tools

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"minimalharness/internal/outputreceipt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultSessionOutputMaxBytes int64 = 512 << 20
const sessionOutputMaxFiles = 4096

// OutputQuotaError stops capture and rejects new commands before they execute
// when the retained session output has consumed its byte or file allowance.
type OutputQuotaError struct {
	Limit int64
	Files bool
}

// BashAdmissionError means capture storage was refused before any command
// execution. Capture-time quota failures remain plain OutputQuotaError values.
type BashAdmissionError struct{ Err error }

func (e *BashAdmissionError) Error() string { return e.Err.Error() }
func (e *BashAdmissionError) Unwrap() error { return e.Err }

func (e *OutputQuotaError) Error() string {
	if e.Files {
		return fmt.Sprintf("session bash output reached the %d-file limit", sessionOutputMaxFiles)
	}
	return fmt.Sprintf("session bash output reached the %d-byte limit", e.Limit)
}

// One owner is selected before starting a goroutine and stays with the capture
// across session switches. The session store holds the cross-process lease.
type bashOutputBudget struct {
	mu        sync.Mutex
	used      int64
	files     int
	limit     int64
	active    map[string]bool
	lastSweep time.Time
}

type bashOutputFile struct {
	path      string
	file      *os.File
	budget    *bashOutputBudget
	legacy    bool
	bytes     int64
	closeOnce sync.Once
}

func (f *bashOutputFile) publicPath() string {
	if f.legacy {
		return ""
	}
	return f.path
}

// ReacquireOutput discards cached counters when a session writer is newly
// acquired. The previous lease was released only after all its captures ended;
// another process may have appended output while this app selected a different
// session. Retained writers must keep their live budget instead.
func (e *Executor) ReacquireOutput(dir string) {
	key, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	e.outMu.Lock()
	delete(e.outputBudgets, key)
	e.outMu.Unlock()
}

func (e *Executor) newBashOutput(ctx context.Context) (*bashOutputFile, error) {
	output, err := e.allocateBashOutput(ctx)
	if err != nil {
		return nil, &BashAdmissionError{Err: err}
	}
	return output, nil
}

func (e *Executor) allocateBashOutput(ctx context.Context) (*bashOutputFile, error) {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	key := e.OutputDir
	if key != "" {
		var err error
		key, err = filepath.Abs(key)
		if err != nil {
			return nil, err
		}
	}
	if e.outputBudgets == nil {
		e.outputBudgets = make(map[string]*bashOutputBudget)
	}
	budget := e.outputBudgets[key]
	if budget == nil {
		limit := e.OutputMaxBytes
		if limit <= 0 {
			limit = DefaultSessionOutputMaxBytes
		}
		budget = &bashOutputBudget{limit: limit, active: make(map[string]bool)}
		if key != "" {
			if err := os.MkdirAll(key, 0700); err != nil {
				return nil, fmt.Errorf("create bash output directory: %w", err)
			}
			if err := budget.load(ctx, key); err != nil {
				return nil, err
			}
		}
		e.outputBudgets[key] = budget
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key != "" && (budget.lastSweep.IsZero() || time.Since(budget.lastSweep) >= time.Hour || budget.used >= budget.limit || budget.files >= sessionOutputMaxFiles) {
		if err := budget.load(ctx, key); err != nil {
			return nil, err
		}
	}
	if budget.used >= budget.limit {
		return nil, &OutputQuotaError{Limit: budget.limit}
	}
	if budget.files >= sessionOutputMaxFiles {
		return nil, &OutputQuotaError{Files: true}
	}
	var f *os.File
	if key == "" {
		var err error
		f, err = os.CreateTemp("", "gmh-bash-*")
		if err != nil {
			return nil, fmt.Errorf("create bash output: %w", err)
		}
	} else {
		root, err := os.OpenRoot(key)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, err
		}
		name := fmt.Sprintf("bash-%x.log", token)
		f, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, fmt.Errorf("create bash output: %w", err)
		}
	}
	budget.files++
	path := f.Name()
	budget.active[filepath.Base(path)] = true
	return &bashOutputFile{path: path, file: f, budget: budget, legacy: key == ""}, nil
}

// Scan metadata in bounded batches. Old output counts immediately on resume;
// unexpected file types fail closed instead of following a link or blocking.
func (b *bashOutputBudget) load(ctx context.Context, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	var used int64
	var files, scanned int
	now := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			scanned++
			if scanned > sessionOutputMaxFiles*4 {
				return &OutputQuotaError{Files: true}
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular bash output: %q", entry.Name())
			}
			if !b.active[entry.Name()] && outputreceipt.Expired(dir, entry.Name(), now) {
				if err := root.Remove(entry.Name()); err != nil {
					return fmt.Errorf("expire bash output: %w", err)
				}
				outputreceipt.Remove(dir, entry.Name())
				continue
			}
			files++
			if files > sessionOutputMaxFiles {
				return &OutputQuotaError{Files: true}
			}
			if info.Size() > b.limit-used {
				return &OutputQuotaError{Limit: b.limit}
			}
			used += info.Size()
		}
		if err == io.EOF {
			b.used, b.files, b.lastSweep = used, files, now
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (f *bashOutputFile) Write(p []byte) (int, error) {
	b := f.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := min(int64(len(p)), b.limit-b.used)
	n, err := f.file.Write(p[:keep])
	b.used += int64(n)
	f.bytes += int64(n)
	if err == nil && n < len(p) {
		err = &OutputQuotaError{Limit: b.limit}
	}
	return n, err
}

func (f *bashOutputFile) Close() {
	f.closeOnce.Do(f.close)
}

func (f *bashOutputFile) close() {
	_ = f.file.Close()
	f.budget.mu.Lock()
	delete(f.budget.active, filepath.Base(f.path))
	f.budget.mu.Unlock()
	if f.legacy {
		// Temporary captures share a concurrent budget but release storage on
		// completion. Persistent output is charged until explicit cleanup.
		if err := os.Remove(f.path); err == nil || os.IsNotExist(err) {
			f.budget.mu.Lock()
			f.budget.used -= f.bytes
			f.budget.files--
			f.budget.mu.Unlock()
		}
	}
}
