// Package checkpoint preserves source facts independently of model summaries.
// A checkpoint describes history; it never grants approval or certifies the
// current workspace. Callers must still consult live policy and verification.
package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"minimalharness/internal/verification"
)

const (
	Version      = 1
	MaxBytes     = 256 << 10
	MaxEntries   = 2048
	MaxTextBytes = 64 << 10
)

// Text retains an exact source statement. Source is a stable transcript record
// identity, not a model-generated paraphrase. The caller classifies human
// instructions as constraints and assistant/user-answer statements as decisions;
// their original wording remains available to interpret scope and supersession.
type Text struct {
	Source  string `json:"source"`
	Content string `json:"content"`
}

// Excerpt keeps both ends of a large observation and its full-content digest.
// Bytes is the original byte count; omitted middle text is never implied present.
type Excerpt struct {
	Head   string `json:"head"`
	Tail   string `json:"tail,omitempty"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func Observe(text string, limit int) (Excerpt, error) {
	if !utf8.ValidString(text) || limit < 8 || limit > MaxTextBytes {
		return Excerpt{}, fmt.Errorf("invalid checkpoint observation or limit")
	}
	sum := sha256.Sum256([]byte(text))
	out := Excerpt{SHA256: hex.EncodeToString(sum[:]), Bytes: len(text)}
	if len(text) <= limit {
		out.Head = text
		return out, nil
	}
	head, tail := limit/2, len(text)-(limit-limit/2)
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	out.Head, out.Tail = text[:head], text[tail:]
	return out, nil
}

type Failure struct {
	Fingerprint string  `json:"fingerprint"`
	Source      string  `json:"source"`
	Tool        string  `json:"tool"`
	Command     string  `json:"command,omitempty"`
	Cwd         string  `json:"cwd,omitempty"`
	Output      Excerpt `json:"output"`
}

type PendingCall struct {
	ID              string `json:"id"`
	Tool            string `json:"tool"`
	ArgumentsSHA256 string `json:"arguments_sha256"`
}

type JobStatus string

const (
	Running JobStatus = "running"
	Done    JobStatus = "done"
	Unknown JobStatus = "unknown"
)

// ID plus Started identifies a job incarnation, including registries that reuse
// display IDs after restart. Running is only an observation at checkpoint time.
type Job struct {
	ID           string    `json:"id"`
	Started      time.Time `json:"started"`
	InvocationID string    `json:"invocation_id,omitempty"`
	Command      string    `json:"command"`
	OutputFile   string    `json:"output_file,omitempty"`
	Status       JobStatus `json:"status"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	Error        string    `json:"error,omitempty"`
	Delivered    bool      `json:"delivered,omitempty"`
}

// Artifact is a reference, never file contents or proof that a mutable path
// still exists. SHA256, when present, identifies immutable captured bytes.
type Artifact struct {
	Kind      string `json:"kind"` // image, tool_output, transcript, workspace_path
	Path      string `json:"path"`
	Source    string `json:"source"`
	SHA256    string `json:"sha256,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type State struct {
	Version      int                     `json:"version"`
	ActiveTask   *Text                   `json:"active_task,omitempty"`
	Constraints  []Text                  `json:"constraints,omitempty"`
	Decisions    []Text                  `json:"decisions,omitempty"`
	Verification []verification.Evidence `json:"verification,omitempty"`
	Failures     []Failure               `json:"failures,omitempty"`
	Pending      []PendingCall           `json:"pending,omitempty"`
	Jobs         []Job                   `json:"jobs,omitempty"`
	Artifacts    []Artifact              `json:"artifacts,omitempty"`
}

// Delta contains facts in observation order, not model proposals. A later
// verification attempt replaces the prior attempt of that named check, even
// when the later attempt failed. Only an observed successful rerun may resolve
// a failure fingerprint. Empty fields never silently erase preserved sources.
type Delta struct {
	ActiveTask       *Text
	Constraints      []Text
	Decisions        []Text
	Verification     []verification.Evidence
	Failures         []Failure
	ResolvedFailures []string
	Pending          []PendingCall
	ResolvedCalls    []string
	Jobs             []Job
	RefreshJobs      bool
	Artifacts        []Artifact
}

// Merge owns its result and leaves both inputs unchanged, including on errors.
// Bounds fail closed instead of evicting old instructions or unresolved work.
func Merge(previous State, d Delta) (State, error) {
	if len(d.Constraints)+len(d.Decisions)+len(d.Verification)+len(d.Failures)+len(d.Pending)+len(d.Jobs)+len(d.Artifacts) > MaxEntries || len(d.ResolvedCalls)+len(d.ResolvedFailures) > MaxEntries {
		return State{}, fmt.Errorf("checkpoint delta exceeds %d entries", MaxEntries)
	}
	if previous.Version == 0 {
		if previous.ActiveTask != nil || len(previous.Constraints)+len(previous.Decisions)+len(previous.Verification)+len(previous.Failures)+len(previous.Pending)+len(previous.Jobs)+len(previous.Artifacts) != 0 {
			return State{}, fmt.Errorf("unversioned checkpoint contains state")
		}
		previous.Version = Version
	}
	encoded, err := Encode(previous)
	if err != nil {
		return State{}, err
	}
	out, err := Decode(encoded)
	if err != nil {
		return State{}, err
	}
	constraints := append([]Text(nil), d.Constraints...)
	if d.ActiveTask != nil {
		constraints = append(constraints, *d.ActiveTask)
	}
	if out.Constraints, err = mergeText(out.Constraints, constraints); err != nil {
		return State{}, err
	}
	if out.Decisions, err = mergeText(out.Decisions, d.Decisions); err != nil {
		return State{}, err
	}
	if d.ActiveTask != nil {
		out.ActiveTask = d.ActiveTask
	}
	out.Verification = upsert(out.Verification, d.Verification, func(e verification.Evidence) string { return e.Check })
	out.Failures = remove(out.Failures, d.ResolvedFailures, func(f Failure) string { return f.Fingerprint })
	out.Failures = upsert(out.Failures, d.Failures, func(f Failure) string { return f.Fingerprint })
	out.Pending = remove(out.Pending, d.ResolvedCalls, func(p PendingCall) string { return p.ID })
	knownCalls := make(map[string]PendingCall, len(out.Pending)+len(d.Pending))
	for _, old := range out.Pending {
		knownCalls[old.ID] = old
	}
	for _, added := range d.Pending {
		if old, ok := knownCalls[added.ID]; ok && old != added {
			return State{}, fmt.Errorf("pending checkpoint call identity changed")
		}
		knownCalls[added.ID] = added
	}
	out.Pending = upsert(out.Pending, d.Pending, func(p PendingCall) string { return p.ID })
	if d.RefreshJobs {
		seen := make(map[string]bool, len(d.Jobs))
		for _, job := range d.Jobs {
			seen[jobKey(job)] = true
		}
		for i := range out.Jobs {
			if out.Jobs[i].Status == Running && !seen[jobKey(out.Jobs[i])] {
				out.Jobs[i].Status = Unknown
			}
		}
	}
	out.Jobs = upsert(out.Jobs, d.Jobs, jobKey)
	out.Artifacts = upsert(out.Artifacts, d.Artifacts, func(a Artifact) string { b, _ := json.Marshal(a); return string(b) })
	encoded, err = Encode(out)
	if err != nil {
		return State{}, err
	}
	return Decode(encoded) // Detach incoming slices and pointer fields too.
}

func mergeText(old, added []Text) ([]Text, error) {
	positions := make(map[string]int, len(old)+len(added))
	for i, item := range old {
		positions[item.Source] = i
	}
	for _, item := range added {
		if i, ok := positions[item.Source]; ok {
			if old[i] != item {
				return nil, fmt.Errorf("checkpoint source %q changed", item.Source)
			}
			continue
		}
		positions[item.Source] = len(old)
		old = append(old, item)
	}
	return old, nil
}

func upsert[T any](old, added []T, key func(T) string) []T {
	positions := make(map[string]int, len(old)+len(added))
	for i, item := range old {
		positions[key(item)] = i
	}
	for _, item := range added {
		k := key(item)
		if i, ok := positions[k]; ok {
			old[i] = item
		} else {
			positions[k] = len(old)
			old = append(old, item)
		}
	}
	return old
}

func remove[T any](old []T, keys []string, key func(T) string) []T {
	removed := make(map[string]bool, len(keys))
	for _, k := range keys {
		removed[k] = true
	}
	out := old[:0]
	for _, item := range old {
		if !removed[key(item)] {
			out = append(out, item)
		}
	}
	return out
}

func jobKey(j Job) string { return j.ID + "\x00" + j.Started.UTC().Format(time.RFC3339Nano) }

func Encode(s State) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("checkpoint exceeds %d encoded bytes", MaxBytes)
	}
	return b, nil
}

func Decode(data []byte) (State, error) {
	if len(data) > MaxBytes {
		return State{}, fmt.Errorf("checkpoint exceeds %d encoded bytes", MaxBytes)
	}
	if !utf8.Valid(data) {
		return State{}, fmt.Errorf("checkpoint is not valid UTF-8")
	}
	var out State
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return State{}, fmt.Errorf("decode checkpoint: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return State{}, fmt.Errorf("trailing checkpoint data")
	}
	if err := out.validate(); err != nil {
		return State{}, err
	}
	return out, nil
}

func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s State) validate() error {
	if s.Version != Version {
		return fmt.Errorf("unsupported checkpoint version %d", s.Version)
	}
	if len(s.Constraints)+len(s.Decisions)+len(s.Verification)+len(s.Failures)+len(s.Pending)+len(s.Jobs)+len(s.Artifacts) > MaxEntries {
		return fmt.Errorf("checkpoint exceeds %d entries", MaxEntries)
	}
	bytes := 0
	text := func(fields ...string) error {
		for _, field := range fields {
			if len(field) > MaxTextBytes || !utf8.ValidString(field) {
				return fmt.Errorf("invalid or oversized checkpoint text")
			}
			bytes += len(field)
			if bytes > MaxBytes {
				return fmt.Errorf("checkpoint exceeds %d text bytes", MaxBytes)
			}
		}
		return nil
	}
	sources := make(map[string]string)
	for _, list := range [][]Text{s.Constraints, s.Decisions} {
		for _, item := range list {
			if item.Source == "" || strings.TrimSpace(item.Content) == "" {
				return fmt.Errorf("checkpoint text requires source and content")
			}
			if err := text(item.Source, item.Content); err != nil {
				return err
			}
			if _, ok := sources[item.Source]; ok {
				return fmt.Errorf("duplicate checkpoint text source")
			}
			sources[item.Source] = item.Content
		}
	}
	if s.ActiveTask != nil {
		found := false
		for _, item := range s.Constraints {
			if item == *s.ActiveTask {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("active task is not a preserved instruction source")
		}
	}
	seen := make(map[string]bool)
	unique := func(kind, key string) error {
		key = kind + "\x00" + key
		if seen[key] {
			return fmt.Errorf("duplicate checkpoint %s key", kind)
		}
		seen[key] = true
		return nil
	}
	for _, e := range s.Verification {
		if e.Check == "" || e.Command == "" || e.Cwd == "" || e.Started.IsZero() || e.Finished.Before(e.Started) || e.ExitCode == nil && e.Error == "" {
			return fmt.Errorf("invalid historical verification evidence")
		}
		if err := text(e.Check, e.Command, e.Cwd, e.Version, e.Error, e.InvocationID, e.ToolCallID); err != nil {
			return err
		}
		if err := unique("verification", e.Check); err != nil {
			return err
		}
	}
	for _, f := range s.Failures {
		if f.Fingerprint == "" || f.Source == "" || f.Tool == "" || !digest(f.Output.SHA256) || f.Output.Bytes < len(f.Output.Head)+len(f.Output.Tail) {
			return fmt.Errorf("invalid checkpoint failure observation")
		}
		if f.Output.Tail == "" && f.Output.Bytes != len(f.Output.Head) || f.Output.Tail != "" && f.Output.Bytes <= len(f.Output.Head)+len(f.Output.Tail) {
			return fmt.Errorf("inconsistent checkpoint excerpt bounds")
		}
		if f.Output.Tail == "" {
			sum := sha256.Sum256([]byte(f.Output.Head))
			if hex.EncodeToString(sum[:]) != f.Output.SHA256 {
				return fmt.Errorf("checkpoint excerpt digest mismatch")
			}
		}
		if err := text(f.Fingerprint, f.Source, f.Tool, f.Command, f.Cwd, f.Output.Head, f.Output.Tail, f.Output.SHA256); err != nil {
			return err
		}
		if err := unique("failure", f.Fingerprint); err != nil {
			return err
		}
	}
	for _, p := range s.Pending {
		if p.ID == "" || p.Tool == "" || !digest(p.ArgumentsSHA256) {
			return fmt.Errorf("invalid pending checkpoint call")
		}
		if err := text(p.ID, p.Tool, p.ArgumentsSHA256); err != nil {
			return err
		}
		if err := unique("pending", p.ID); err != nil {
			return err
		}
	}
	for _, j := range s.Jobs {
		if j.ID == "" || j.Started.IsZero() || j.Command == "" {
			return fmt.Errorf("checkpoint job requires identity and command")
		}
		switch j.Status {
		case Running, Unknown:
			if j.ExitCode != nil || j.Error != "" || j.Delivered {
				return fmt.Errorf("unfinished checkpoint job claims a delivered result")
			}
		case Done:
			if j.ExitCode == nil && j.Error == "" {
				return fmt.Errorf("finished checkpoint job has no result")
			}
		default:
			return fmt.Errorf("unknown checkpoint job status")
		}
		if err := text(j.ID, j.InvocationID, j.Command, j.OutputFile, j.Error); err != nil {
			return err
		}
		if err := unique("job", jobKey(j)); err != nil {
			return err
		}
	}
	for _, a := range s.Artifacts {
		switch a.Kind {
		case "image", "tool_output", "transcript", "workspace_path":
		default:
			return fmt.Errorf("unknown checkpoint artifact kind")
		}
		if a.Path == "" || a.Source == "" || a.SHA256 != "" && !digest(a.SHA256) {
			return fmt.Errorf("invalid checkpoint artifact reference")
		}
		if err := text(a.Kind, a.Path, a.Source, a.SHA256, a.MediaType); err != nil {
			return err
		}
		b, _ := json.Marshal(a)
		if err := unique("artifact", string(b)); err != nil {
			return err
		}
	}
	return nil
}
