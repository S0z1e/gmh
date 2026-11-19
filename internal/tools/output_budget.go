package tools

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const DefaultSessionOutputMaxBytes int64 = 512 << 20
const sessionOutputMaxFiles = 4096

// OutputQuotaError stops capture and rejects new commands before they execute
// when the retained session output has consumed its byte or file allowance.
type OutputQuotaError struct {
	Limit int64
	Files bool
}

func (e *OutputQuotaError) Error() string {
	if e.Files {
		return fmt.Sprintf("session bash output reached the %d-file limit", sessionOutputMaxFiles)
	}
	return fmt.Sprintf("session bash output reached the %d-byte limit", e.Limit)
}

// One owner is selected before starting a goroutine and stays with the capture
// across session switches. The session store holds the cross-process lease.
type bashOutputBudget struct {
	mu    sync.Mutex
	used  int64
	files int
	limit int64
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
		budget = &bashOutputBudget{limit: limit}
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
	return &bashOutputFile{path: path, file: f, budget: budget, legacy: key == ""}, nil
}

// Scan metadata in bounded batches. Old output counts immediately on resume;
// unexpected file types fail closed instead of following a link or blocking.
func (b *bashOutputBudget) load(ctx context.Context, dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular bash output: %q", entry.Name())
			}
			b.files++
			if info.Size() > b.limit-b.used {
				return &OutputQuotaError{Limit: b.limit}
			}
			b.used += info.Size()
			if b.files > sessionOutputMaxFiles {
				return &OutputQuotaError{Files: true}
			}
		}
		if err == io.EOF {
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
