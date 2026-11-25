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
	"slices"
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
	Interrupted bool    `json:"interrupted,omitempty"`
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
	ID              string    `json:"id"`
	Started         time.Time `json:"started"`
	InvocationID    string    `json:"invocation_id,omitempty"`
	Command         string    `json:"command"`
	Cwd             string    `json:"cwd,omitempty"`
	OutputFile      string    `json:"output_file,omitempty"`
	OutputID        string    `json:"output_id,omitempty"`
	OutputReceipt   string    `json:"output_receipt,omitempty"`
	PID             int       `json:"pid,omitempty"`
	PGID            int       `json:"pgid,omitempty"`
	ProcessStarted  string    `json:"process_started,omitempty"`
	Status          JobStatus `json:"status"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Error           string    `json:"error,omitempty"`
	Delivered       bool      `json:"delivered,omitempty"`
	AdoptionNoticed bool      `json:"adoption_noticed,omitempty"`
	UnknownNoticed  bool      `json:"unknown_noticed,omitempty"`
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
	if err := d.validate(true); err != nil {
		return State{}, err
	}
	return mergeValidated(previous, d, false)
}

// EvictionStats reports how many records of each class were dropped to keep
// a checkpoint within MaxEntries and MaxBytes.
type EvictionStats struct {
	Artifacts           int
	Jobs                int
	Decisions           int
	InterruptedFailures int
	Failures            int
}

func (e EvictionStats) Total() int {
	return e.Artifacts + e.Jobs + e.Decisions + e.InterruptedFailures + e.Failures
}

func (e *EvictionStats) Add(other EvictionStats) {
	e.Artifacts += other.Artifacts
	e.Jobs += other.Jobs
	e.Decisions += other.Decisions
	e.InterruptedFailures += other.InterruptedFailures
	e.Failures += other.Failures
}

func (e EvictionStats) String() string {
	return fmt.Sprintf("artifacts=%d jobs=%d decisions=%d interrupted_failures=%d failures=%d",
		e.Artifacts, e.Jobs, e.Decisions, e.InterruptedFailures, e.Failures)
}

// MergeWithEviction behaves like Merge, but when the combined state exceeds
// MaxEntries or MaxBytes it deterministically evicts the oldest lowest-priority
// historical records (artifacts, delivered jobs, finished/unknown jobs,
// decisions, interrupted unknown failures, and oldest failures) while
// preserving ActiveTask, Constraints, Pending, Verification, and Running jobs.
func MergeWithEviction(previous State, d Delta) (State, error) {
	out, _, err := MergeWithEvictionStats(previous, d)
	return out, err
}

// MergeWithEvictionStats is MergeWithEviction returning structured eviction counts.
func MergeWithEvictionStats(previous State, d Delta) (State, EvictionStats, error) {
	if out, err := Merge(previous, d); err == nil {
		return out, EvictionStats{}, nil
	}
	if err := d.validate(false); err != nil {
		return State{}, EvictionStats{}, err
	}
	var stats EvictionStats
	out, err := mergeValidatedWithStats(previous, d, true, &stats)
	return out, stats, err
}

func mergeValidated(previous State, d Delta, allowEviction bool) (State, error) {
	return mergeValidatedWithStats(previous, d, allowEviction, nil)
}

func mergeValidatedWithStats(previous State, d Delta, allowEviction bool, stats *EvictionStats) (State, error) {
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
	if allowEviction {
		encoded, err = encodeWithEviction(&out, MaxBytes, stats, true)
	} else {
		encoded, err = Encode(out)
	}
	if err != nil {
		return State{}, err
	}
	return Decode(encoded) // Detach incoming slices and pointer fields too.
}

// EncodeForSummary encodes an independent view of the checkpoint within the
// summary's available bytes, retaining the full state for persistence.
// Unlike MergeWithEviction, EncodeForSummary never drops unresolved Failures.
func EncodeForSummary(s State, maxBytes int) ([]byte, error) {
	// Eviction may move job entries in place.
	s.Jobs = slices.Clone(s.Jobs)
	return encodeWithEviction(&s, maxBytes, nil, false)
}

func encodeWithEviction(s *State, maxBytes int, stats *EvictionStats, evictFailures bool) ([]byte, error) {
	for {
		encoded, err := Encode(*s)
		if err == nil {
			if len(encoded) <= maxBytes {
				return encoded, nil
			}
			err = fmt.Errorf("checkpoint exceeds %d bytes", maxBytes)
		}
		if !evictOneOldest(s, stats, evictFailures) {
			return nil, err
		}
	}
}

// evictOneOldest drops one oldest low-priority historical entry so compaction
// can degrade gracefully instead of aborting long turns. ActiveTask,
// Constraints, Pending, Verification, and Running jobs are never evicted.
// When evictFailures is true, interrupted unknown failures are evicted before
// confirmed failures.
func evictOneOldest(s *State, stats *EvictionStats, evictFailures bool) bool {
	if len(s.Artifacts) > 0 {
		s.Artifacts = s.Artifacts[1:]
		if stats != nil {
			stats.Artifacts++
		}
		return true
	}
	for i, j := range s.Jobs {
		if j.Status == Done && j.Delivered {
			s.Jobs = append(s.Jobs[:i], s.Jobs[i+1:]...)
			if stats != nil {
				stats.Jobs++
			}
			return true
		}
	}
	for i, j := range s.Jobs {
		if j.Status == Done || j.Status == Unknown {
			s.Jobs = append(s.Jobs[:i], s.Jobs[i+1:]...)
			if stats != nil {
				stats.Jobs++
			}
			return true
		}
	}
	if len(s.Decisions) > 0 {
		s.Decisions = s.Decisions[1:]
		if stats != nil {
			stats.Decisions++
		}
		return true
	}
	if !evictFailures {
		return false
	}
	for i, f := range s.Failures {
		if f.Interrupted {
			s.Failures = append(s.Failures[:i], s.Failures[i+1:]...)
			if stats != nil {
				stats.InterruptedFailures++
			}
			return true
		}
	}
	if len(s.Failures) > 0 {
		s.Failures = s.Failures[1:]
		if stats != nil {
			stats.Failures++
		}
		return true
	}
	return false
}

// Check incoming facts before upsert/removal can hide an invalid record behind
// a later observation. The limit covers all inputs, including resolutions.
func (d Delta) validate(enforceEntryLimit bool) error {
	entries := len(d.Constraints) + len(d.Decisions) + len(d.Verification) + len(d.Failures) + len(d.Pending) + len(d.Jobs) + len(d.Artifacts) + len(d.ResolvedCalls) + len(d.ResolvedFailures)
	if d.ActiveTask != nil {
		entries++
	}
	if enforceEntryLimit && entries > MaxEntries {
		return fmt.Errorf("checkpoint delta exceeds %d entries", MaxEntries)
	}
	check := func(s State) error { s.Version = Version; _, err := Encode(s); return err }
	if d.ActiveTask != nil {
		if err := check(State{ActiveTask: d.ActiveTask, Constraints: []Text{*d.ActiveTask}}); err != nil {
			return err
		}
	}
	for _, list := range [][]Text{d.Constraints, d.Decisions} {
		for _, record := range list {
			if err := check(State{Constraints: []Text{record}}); err != nil {
				return err
			}
		}
	}
	for _, record := range d.Verification {
		if err := check(State{Verification: []verification.Evidence{record}}); err != nil {
			return err
		}
	}
	for _, record := range d.Failures {
		if err := check(State{Failures: []Failure{record}}); err != nil {
			return err
		}
	}
	for _, record := range d.Pending {
		if err := check(State{Pending: []PendingCall{record}}); err != nil {
			return err
		}
	}
	for _, record := range d.Jobs {
		if err := check(State{Jobs: []Job{record}}); err != nil {
			return err
		}
	}
	for _, record := range d.Artifacts {
		if err := check(State{Artifacts: []Artifact{record}}); err != nil {
			return err
		}
	}
	for _, keys := range [][]string{d.ResolvedCalls, d.ResolvedFailures} {
		for _, key := range keys {
			if key == "" || len(key) > MaxTextBytes || !utf8.ValidString(key) {
				return fmt.Errorf("invalid checkpoint resolution key")
			}
		}
	}
	return nil
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
	if err := validateUnicodeEscapes(data); err != nil {
		return State{}, err
	}
	if err := validateJSONFields(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return State{}, err
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
	// A compact input may expand under the canonical encoder (for example
	// literal HTML characters). Every decoded state must remain re-encodable.
	if _, err := Encode(out); err != nil {
		return State{}, err
	}
	return out, nil
}

func validateJSONFields(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("checkpoint JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid checkpoint JSON field")
			}
			// All version-one field names use lowercase ASCII. Reject aliases
			// before encoding/json's Unicode folding can overwrite a field.
			for _, c := range name {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
					return fmt.Errorf("noncanonical checkpoint JSON field %q", name)
				}
			}
			if seen[name] {
				return fmt.Errorf("duplicate checkpoint JSON field %q", name)
			}
			seen[name] = true
			if err := validateJSONFields(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := validateJSONFields(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid checkpoint JSON delimiter")
	}
	_, err = d.Token()
	return err
}

// encoding/json repairs unpaired UTF-16 escapes with U+FFFD. Reject those
// escapes before decoding so a source statement can never change silently.
func validateUnicodeEscapes(data []byte) error {
	hex4 := func(p []byte) (uint16, error) {
		var n uint16
		for _, c := range p {
			var digit byte
			switch {
			case c >= '0' && c <= '9':
				digit = c - '0'
			case c >= 'a' && c <= 'f':
				digit = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				digit = c - 'A' + 10
			default:
				return 0, fmt.Errorf("invalid checkpoint Unicode escape")
			}
			n = n*16 + uint16(digit)
		}
		return n, nil
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		for i++; i < len(data) && data[i] != '"'; i++ {
			if data[i] != '\\' {
				continue
			}
			i++
			if i >= len(data) || data[i] != 'u' {
				continue
			}
			if i+4 >= len(data) {
				return fmt.Errorf("incomplete checkpoint Unicode escape")
			}
			n, err := hex4(data[i+1 : i+5])
			if err != nil {
				return err
			}
			i += 4
			if n >= 0xd800 && n <= 0xdbff {
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return fmt.Errorf("unpaired checkpoint Unicode surrogate")
				}
				low, err := hex4(data[i+3 : i+7])
				if err != nil {
					return err
				}
				if low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("unpaired checkpoint Unicode surrogate")
				}
				i += 6
			} else if n >= 0xdc00 && n <= 0xdfff {
				return fmt.Errorf("unpaired checkpoint Unicode surrogate")
			}
		}
	}
	return nil
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
		if j.PID < 0 || j.PGID < 0 {
			return fmt.Errorf("checkpoint job process identity must be non-negative")
		}
		switch j.Status {
		case Running, Unknown:
			if j.ExitCode != nil || j.Error != "" || j.Delivered || j.OutputReceipt != "" {
				return fmt.Errorf("unfinished checkpoint job claims a delivered result")
			}
		case Done:
			if j.ExitCode == nil && j.Error == "" {
				return fmt.Errorf("finished checkpoint job has no result")
			}
			if !j.Delivered && j.OutputReceipt != "" {
				return fmt.Errorf("undelivered checkpoint job claims an output receipt")
			}
		default:
			return fmt.Errorf("unknown checkpoint job status")
		}
		if err := text(j.ID, j.InvocationID, j.Command, j.Cwd, j.OutputFile, j.OutputID, j.OutputReceipt, j.ProcessStarted, j.Error); err != nil {
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
