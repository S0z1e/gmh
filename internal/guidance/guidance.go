// Package guidance resolves trusted, target-scoped project instruction sources.
// It returns source identities and exact text, never permissions to execute.
package guidance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"minimalharness/internal/fileguard"
	"minimalharness/internal/textutil"
)

const (
	PrefixBytes       = 8 << 10
	MaxSourceBytes    = 1 << 20
	MaxTotalBytes     = 8 << 20
	MaxDirectories    = 64
	MaxAncestorLevels = 8
)

var names = []string{"AGENTS.md", "CLAUDE.md"}

type File struct {
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Prefix string `json:"prefix"`
	More   bool   `json:"more"`
}

// Files are ordered from the furthest allowed ancestor through the workspace
// to the nearest target directory. Later files refine earlier files within
// their scope; CLAUDE.md follows AGENTS.md at the same directory.
type Snapshot struct {
	Root   string `json:"root"`
	Target string `json:"target"`
	Files  []File `json:"files"`
	SHA256 string `json:"sha256"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func contained(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func directories(root, target string) ([]string, string, error) {
	dir := root
	resolved := root
	if target != "" && target != "." {
		binding, err := fileguard.Prepare(root, target)
		if err != nil {
			return nil, "", err
		}
		resolved = filepath.Join(root, binding.Path)
		binding.Close()
		dir = filepath.Dir(resolved)
	}
	var ancestors []string
	home, _ := os.UserHomeDir()
	if home != "" {
		if value, err := canonicalRoot(home); err == nil {
			home = value
		} else {
			home = ""
		}
	}
	for parent, level := root, 0; level < MaxAncestorLevels; level++ {
		ancestors = append(ancestors, parent)
		next := filepath.Dir(parent)
		if next == parent || home == "" || !contained(home, next) {
			break
		}
		parent = next
	}
	slices.Reverse(ancestors)
	var descendants []string
	for at := dir; at != root; at = filepath.Dir(at) {
		if !contained(root, at) {
			return nil, "", fmt.Errorf("guidance target outside workspace")
		}
		descendants = append(descendants, at)
		if len(ancestors)+len(descendants) > MaxDirectories {
			return nil, "", fmt.Errorf("guidance scope exceeds %d directories", MaxDirectories)
		}
	}
	slices.Reverse(descendants)
	return append(ancestors, descendants...), resolved, nil
}

// Resolve loads bounded regular files, follows the effective target's in-root
// symlinks, and never loads instructions from sibling directories. Missing
// sources are absent; unreadable, special or oversized sources fail explicitly.
func Resolve(ctx context.Context, root, target string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	requestedRoot, err := filepath.Abs(root)
	if err != nil {
		return Snapshot{}, err
	}
	if filepath.IsAbs(target) && contained(requestedRoot, target) {
		target, err = filepath.Rel(requestedRoot, target)
		if err != nil {
			return Snapshot{}, err
		}
	}
	root, err = canonicalRoot(root)
	if err != nil {
		return Snapshot{}, err
	}
	dirs, resolved, err := directories(root, target)
	if err != nil {
		return Snapshot{}, err
	}
	out := Snapshot{Root: root, Target: resolved}
	total := 0
	for _, dir := range dirs {
		for _, name := range names {
			data, err := readSource(ctx, dir, name)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return Snapshot{}, fmt.Errorf("guidance %s: %w", filepath.Join(dir, name), err)
			}
			total += len(data)
			if total > MaxTotalBytes {
				return Snapshot{}, fmt.Errorf("guidance sources exceed %d bytes", MaxTotalBytes)
			}
			prefix := textutil.Prefix(string(data), PrefixBytes)
			out.Files = append(out.Files, File{Path: filepath.Join(dir, name), Scope: dir, SHA256: digest(data), Bytes: len(data), Prefix: prefix, More: len(prefix) < len(data)})
		}
	}
	// The full byte digest of each source already commits to all its text.
	manifest := out
	manifest.Files = slices.Clone(out.Files)
	for i := range manifest.Files {
		manifest.Files[i].Prefix = ""
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return Snapshot{}, err
	}
	out.SHA256 = digest(data)
	return out, nil
}

func readSource(ctx context.Context, dir, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, rel, err := fileguard.Root(dir, name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(rel, os.O_RDONLY|nonblockRead, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("instruction source must be a regular file")
	}
	if info.Size() > MaxSourceBytes {
		return nil, fmt.Errorf("instruction source exceeds %d bytes", MaxSourceBytes)
	}
	var data []byte
	reader := io.LimitReader(file, MaxSourceBytes+1)
	buf := make([]byte, 16<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > MaxSourceBytes {
			return nil, fmt.Errorf("instruction source exceeds %d bytes", MaxSourceBytes)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || int64(len(data)) != after.Size() {
		return nil, fmt.Errorf("instruction source changed while reading")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("instruction source is not valid UTF-8")
	}
	return data, nil
}

type Chunk struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Offset int    `json:"offset"`
	Next   int    `json:"next"`
	Bytes  int    `json:"bytes"`
	Text   string `json:"text"`
	More   bool   `json:"more"`
}

// Read returns more of one applicable source, bound to the reviewed full-source
// hash. It cannot become a general read exception for unrelated ancestor files.
// Offset and Next are byte offsets and must lie on UTF8 boundaries.
func Read(ctx context.Context, root, target, path, expectedHash string, offset, limit int) (Chunk, error) {
	if offset < 0 || limit < 4 || limit > PrefixBytes {
		return Chunk{}, fmt.Errorf("invalid guidance byte range")
	}
	snapshot, err := Resolve(ctx, root, target)
	if err != nil {
		return Chunk{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Chunk{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return Chunk{}, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	for _, source := range snapshot.Files {
		if source.Path != path {
			continue
		}
		if source.SHA256 != expectedHash {
			return Chunk{}, fmt.Errorf("instruction source changed; refresh guidance before continuing")
		}
		data, err := readSource(ctx, source.Scope, filepath.Base(source.Path))
		if err != nil {
			return Chunk{}, err
		}
		if digest(data) != expectedHash {
			return Chunk{}, fmt.Errorf("instruction source changed; refresh guidance before continuing")
		}
		if offset > len(data) || offset < len(data) && !utf8.RuneStart(data[offset]) {
			return Chunk{}, fmt.Errorf("invalid guidance offset")
		}
		text := textutil.Prefix(string(data[offset:]), limit)
		next := offset + len(text)
		return Chunk{Path: source.Path, SHA256: expectedHash, Offset: offset, Next: next, Bytes: len(data), Text: text, More: next < len(data)}, nil
	}
	return Chunk{}, fmt.Errorf("instruction source does not apply to target")
}
