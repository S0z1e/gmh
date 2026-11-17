// Package guidance resolves trusted, target-scoped project instruction sources.
// It returns source identities and exact text, never permissions to execute.
package guidance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func directories(handle *os.Root, root, target, home string) ([]string, string, error) {
	dir := root
	resolved := root
	if target != "" && target != "." {
		binding, err := fileguard.ResolveTarget(handle, target)
		if err != nil {
			return nil, "", err
		}
		resolved = filepath.Join(root, binding.Path)
		binding.Close()
		dir = filepath.Dir(resolved)
	}
	var ancestors []string
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
	if !utf8.ValidString(root) || !utf8.ValidString(target) {
		return Snapshot{}, fmt.Errorf("guidance paths must be valid UTF-8")
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
	if !utf8.ValidString(root) {
		return Snapshot{}, fmt.Errorf("guidance root must be valid UTF-8")
	}
	if filepath.IsAbs(target) && contained(root, target) {
		target, err = filepath.Rel(root, target)
		if err != nil {
			return Snapshot{}, err
		}
	}
	roots, err := openSourceRoots(root)
	if err != nil {
		return Snapshot{}, err
	}
	defer roots.close()
	dirs, resolved, err := directories(roots.workspace, root, target, roots.homePath)
	if err != nil {
		return Snapshot{}, err
	}
	out := Snapshot{Root: root, Target: resolved}
	total := 0
	for _, dir := range dirs {
		for _, name := range names {
			data, err := roots.read(ctx, dir, name)
			if errors.Is(err, errSourceAbsent) {
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
	if err := roots.check(); err != nil {
		return Snapshot{}, err
	}
	_, currentTarget, err := directories(roots.workspace, root, target, roots.homePath)
	if err != nil || currentTarget != resolved {
		return Snapshot{}, fmt.Errorf("guidance target changed during discovery")
	}
	if !utf8.ValidString(resolved) {
		return Snapshot{}, fmt.Errorf("guidance target must be valid UTF-8")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
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

var errSourceAbsent = errors.New("instruction source absent")

type sourceRoots struct {
	workspace, home         *os.Root
	workspacePath, homePath string
	workspaceInfo, homeInfo os.FileInfo
}

func openSourceRoots(root string) (*sourceRoots, error) {
	out := &sourceRoots{workspacePath: root}
	var err error
	out.workspace, err = os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	out.workspaceInfo, err = out.workspace.Stat(".")
	if err != nil {
		out.close()
		return nil, err
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if canonical, err := canonicalRoot(home); err == nil && contained(canonical, root) {
			out.homePath = canonical
			if canonical != root {
				out.home, err = os.OpenRoot(canonical)
				if err != nil {
					out.close()
					return nil, err
				}
				out.homeInfo, err = out.home.Stat(".")
				if err != nil {
					out.close()
					return nil, err
				}
			}
		}
	}
	return out, nil
}

func (r *sourceRoots) close() {
	if r.home != nil {
		r.home.Close()
	}
	if r.workspace != nil {
		r.workspace.Close()
	}
}

func (r *sourceRoots) check() error {
	for _, entry := range []struct {
		path string
		info os.FileInfo
	}{{r.workspacePath, r.workspaceInfo}, {r.homePath, r.homeInfo}} {
		if entry.info == nil {
			continue
		}
		current, err := os.Stat(entry.path)
		if err != nil {
			return err
		}
		if !os.SameFile(entry.info, current) {
			return fmt.Errorf("instruction scope root changed while reading")
		}
	}
	return nil
}

func (r *sourceRoots) read(ctx context.Context, dir, name string) ([]byte, error) {
	base, handle := r.workspacePath, r.workspace
	if !contained(base, dir) {
		base, handle = r.homePath, r.home
		if handle == nil || !contained(base, dir) {
			return nil, fmt.Errorf("instruction scope outside allowed roots")
		}
	}
	relative, err := filepath.Rel(base, dir)
	if err != nil {
		return nil, err
	}
	observed, err := handle.Lstat(relative)
	if os.IsNotExist(err) {
		return nil, errSourceAbsent
	}
	if err != nil {
		return nil, err
	}
	if !observed.IsDir() {
		return nil, fmt.Errorf("instruction scope changed during discovery")
	}
	scope, err := handle.OpenRoot(relative)
	if err != nil {
		return nil, err
	}
	defer scope.Close()
	actual, err := scope.Stat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(observed, actual) {
		return nil, fmt.Errorf("instruction scope changed during discovery")
	}
	data, readErr := readSource(ctx, scope, name)
	current, err := handle.Stat(relative)
	if err != nil || !os.SameFile(actual, current) {
		return nil, fmt.Errorf("instruction scope changed while reading")
	}
	return data, readErr
}

func readSource(ctx context.Context, root *os.Root, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := root.Lstat(rel); err != nil {
		if os.IsNotExist(err) {
			return nil, errSourceAbsent
		}
		return nil, err
	}
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
	change, err := changeTime(file, info)
	if err != nil {
		return nil, err
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
	afterChange, err := changeTime(file, after)
	if err != nil {
		return nil, err
	}
	if change != afterChange || after.Mode() != info.Mode() || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || int64(len(data)) != after.Size() {
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
	if !utf8.ValidString(path) {
		return Chunk{}, fmt.Errorf("guidance source path must be valid UTF-8")
	}
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
		roots, err := openSourceRoots(snapshot.Root)
		if err != nil {
			return Chunk{}, err
		}
		data, err := roots.read(ctx, source.Scope, filepath.Base(source.Path))
		if err == nil {
			err = roots.check()
		}
		roots.close()
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
		if err := ctx.Err(); err != nil {
			return Chunk{}, err
		}
		return Chunk{Path: source.Path, SHA256: expectedHash, Offset: offset, Next: next, Bytes: len(data), Text: text, More: next < len(data)}, nil
	}
	return Chunk{}, fmt.Errorf("instruction source does not apply to target")
}
