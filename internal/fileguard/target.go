package fileguard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"minimalharness/internal/workspace"
)

// Target binds a mutation to a parent directory and leaf name, following
// symlinks during preparation, never when committing the replacement.
// Path is relative to the workspace root; Info is nil for a new leaf.
// Close releases all directory handles retained through an approval wait.
type Target struct {
	Path   string
	Name   string
	Parent *os.Root
	Info   os.FileInfo
	// Aliases retains intermediate spellings while expanding symlinks,
	// including protected names whose referents do not exist yet.
	Aliases   []string
	root      *os.Root
	requested string
	parents   []*os.Root
	missing   []string
	ownsRoot  bool
}

func Prepare(base, path string) (*Target, error) {
	r, rel, err := Root(base, path)
	if err != nil {
		return nil, err
	}
	t, err := ResolveTarget(r, rel)
	if err != nil {
		r.Close()
		return nil, err
	}
	t.ownsRoot = true
	return t, nil
}

// ResolveTarget borrows r; closing the target closes only its child handles.
func ResolveTarget(r *os.Root, name string) (target *Target, err error) {
	return resolveTarget(r, name, false)
}

func resolveTarget(r *os.Root, name string, allowDirectory bool) (target *Target, err error) {
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return nil, fmt.Errorf("target must be relative to workspace")
	}
	t := &Target{root: r, requested: name, parents: []*os.Root{r}, Aliases: []string{name}}
	defer func() {
		if err != nil {
			t.Close()
		}
	}()
	parts := strings.Split(filepath.ToSlash(name), "/")
	var names []string
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(t.parents) == 1 {
				return nil, fmt.Errorf("target outside workspace")
			}
			t.parents[len(t.parents)-1].Close()
			t.parents = t.parents[:len(t.parents)-1]
			names = names[:len(names)-1]
			continue
		}
		parent := t.parents[len(t.parents)-1]
		info, statErr := parent.Lstat(part)
		if os.IsNotExist(statErr) {
			remaining := append([]string{part}, parts...)
			var missing []string
			for _, p := range remaining {
				if p == "" || p == "." {
					continue
				}
				if p == ".." {
					return nil, fmt.Errorf("cannot resolve missing parent before ..")
				}
				missing = append(missing, p)
			}
			t.Name = missing[len(missing)-1]
			t.missing = missing[:len(missing)-1]
			t.Parent = parent
			t.Path = filepath.Join(append(slices.Clone(names), missing...)...)
			return t, nil
		}
		if statErr != nil {
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return nil, fmt.Errorf("too many symlinks: %s", name)
			}
			link, err := parent.Readlink(part)
			if err != nil {
				return nil, err
			}
			if filepath.IsAbs(link) || filepath.VolumeName(link) != "" {
				return nil, fmt.Errorf("absolute symlink target is outside workspace: %s", link)
			}
			parts = append(strings.Split(filepath.ToSlash(link), "/"), parts...)
			t.Aliases = append(t.Aliases, filepath.Join(append(slices.Clone(names), parts...)...))
			continue
		}
		if len(parts) == 0 {
			if !info.Mode().IsRegular() && !(allowDirectory && info.IsDir()) {
				return nil, fmt.Errorf("cannot replace non-regular file: %s", name)
			}
			t.Parent, t.Name, t.Info = parent, part, info
			t.Path = filepath.Join(append(slices.Clone(names), part)...)
			return t, nil
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("not a directory: %s", part)
		}
		child, err := openObservedDirectory(parent, part, info)
		if err != nil {
			return nil, err
		}
		t.parents = append(t.parents, child)
		names = append(names, part)
	}
	if !allowDirectory {
		return nil, fmt.Errorf("target names a directory")
	}
	// A directory link may finish in '.', '..', or a trailing separator.
	// These consume the path without reaching the normal leaf branch.
	t.Parent = t.parents[len(t.parents)-1]
	t.Name, t.Path = ".", filepath.Join(names...)
	if t.Path == "" {
		t.Path = "."
	}
	t.Info, err = t.Parent.Stat(".")
	if err != nil {
		return nil, err
	}
	return t, nil
}

// OpenRoot may follow a symlink inserted after Lstat. Compare the opened
// descriptor with the observed directory so such a swap cannot redirect us.
func openObservedDirectory(parent *os.Root, name string, observed os.FileInfo) (*os.Root, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := child.Stat(".")
	if err != nil || !os.SameFile(observed, actual) {
		child.Close()
		return nil, fmt.Errorf("mutation target changed while resolving %s", name)
	}
	return child, nil
}

func (t *Target) Close() error {
	var first error
	for i := len(t.parents) - 1; i >= 0; i-- {
		if i == 0 && !t.ownsRoot {
			continue
		}
		if err := t.parents[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	t.parents = nil
	return first
}

// CheckRootFileID compares the pinned mutation root to the trusted workspace.
func (t *Target) CheckRootFileID(expected string) error {
	return workspace.CheckRootFileID(t.root, expected)
}

// Check detects retargeting during approval. Even a swap after Check cannot
// redirect a commit: the writer uses Parent and Name, never reopens the alias.
func (t *Target) Check() error {
	next, err := ResolveTarget(t.root, t.requested)
	if err != nil {
		return fmt.Errorf("mutation target changed: %w", err)
	}
	defer next.Close()
	oldParent, err := t.Parent.Stat(".")
	if err != nil {
		return err
	}
	newParent, err := next.Parent.Stat(".")
	if err != nil {
		return err
	}
	sameLeaf := t.Info == nil && next.Info == nil || t.Info != nil && next.Info != nil && os.SameFile(t.Info, next.Info)
	if t.Path != next.Path || t.Name != next.Name || !slices.Equal(t.missing, next.missing) || !os.SameFile(oldParent, newParent) || !sameLeaf {
		return fmt.Errorf("mutation target changed after preparation")
	}
	// Preserve current permissions if they changed during the approval wait.
	t.Info = next.Info
	return nil
}

// CreateParents runs only after approval. Newly inserted symlinks are refused;
// each newly created directory is opened and pinned before descending further.
func (t *Target) CreateParents() error {
	if err := t.Check(); err != nil {
		return err
	}
	for _, name := range t.missing {
		if err := t.Parent.Mkdir(name, 0755); err != nil {
			return fmt.Errorf("mutation parent changed or cannot be created: %w", err)
		}
		info, err := t.Parent.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("mutation parent changed: %s", name)
		}
		child, err := openObservedDirectory(t.Parent, name, info)
		if err != nil {
			return err
		}
		t.parents = append(t.parents, child)
		t.Parent = child
	}
	t.missing = nil
	return nil
}

func (t *Target) ReadFile() ([]byte, error) {
	if err := t.Check(); err != nil {
		return nil, err
	}
	if t.Info == nil {
		return nil, os.ErrNotExist
	}
	f, err := t.Parent.Open(t.Name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(t.Info, info) {
		return nil, fmt.Errorf("mutation target changed before reading")
	}
	return io.ReadAll(f)
}

// Touches recognizes filesystem aliases of a protected file or directory,
// including case aliases and alternate names of a protected root itself.
func (t *Target) Touches(path string, directory bool) bool {
	// Use the same resolver and symlink limit as the mutation itself.
	resolved, err := resolveTarget(t.root, path, directory)
	if err != nil {
		// An unresolvable protected root is an unknown policy boundary.
		// Require approval instead of silently classifying it as safe.
		return true
	}
	defer resolved.Close()
	protected := resolved.Info
	if protected == nil {
		// No inode exists yet. Conservatively recognize case and Unicode
		// canonical equivalents, including normalization-insensitive macOS.
		want := normalizedPath(resolved.Path)
		got := normalizedPath(t.Path)
		return got == want || directory && strings.HasPrefix(got, want+"/")
	}
	if t.Info != nil && os.SameFile(t.Info, protected) {
		return true
	}
	if protected.IsDir() {
		for _, parent := range t.parents {
			info, err := parent.Stat(".")
			if err == nil && os.SameFile(info, protected) {
				return true
			}
		}
	}
	return false
}

func normalizedPath(path string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(filepath.ToSlash(path))))
}
