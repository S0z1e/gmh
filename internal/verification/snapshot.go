package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"minimalharness/internal/workspace"
)

const (
	MaxEntries   = 50000
	MaxBytes     = 128 << 20
	MaxFileBytes = 16 << 20
	MaxPathBytes = 8 << 20
	maxDepth     = 256
)

type Snapshot struct {
	Root    workspace.Identity `json:"root"`
	Version string             `json:"version"`
	Files   int                `json:"files"`
	Bytes   int64              `json:"bytes"`
}

type fingerprint struct {
	path string
	info os.FileInfo
}
type contentRecord struct {
	path string
	mode os.FileMode
	sum  [32]byte
}

// Capture hashes regular workspace files and executable bits, ignoring .git and
// explicitly excluded relative paths. Unsupported files, limits, cancellation or
// concurrent changes are errors, never an apparently complete truncated version.
// Symlinks must be explicitly excluded; their outside/recursive inputs cannot be
// represented as ordinary workspace files. Reading never executes project code.
func Capture(ctx context.Context, root string, exclude []string) (Snapshot, error) {
	var result Snapshot
	identity, err := workspace.Identify(root)
	if err != nil {
		return result, err
	}
	if identity.FileID == "" {
		return result, fmt.Errorf("verification requires a workspace creation identity")
	}
	excludes := []string{".git"}
	for _, path := range exclude {
		clean := filepath.Clean(path)
		if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return result, fmt.Errorf("verification exclusion must be a relative path within the workspace")
		}
		excludes = append(excludes, filepath.ToSlash(clean))
	}
	sort.Strings(excludes)
	skip := func(path string) bool {
		for _, excluded := range excludes {
			if path == excluded || strings.HasPrefix(path, excluded+"/") {
				return true
			}
		}
		return false
	}
	handle, err := os.OpenRoot(identity.Path)
	if err != nil {
		return result, err
	}
	defer handle.Close()
	result.Root = identity
	var records []contentRecord
	var fingerprints []fingerprint
	entries := 0
	pathBytes := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("verification workspace exceeds directory depth limit")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := handle.OpenFile(filepath.FromSlash(dir), os.O_RDONLY|nonblock, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		before, err := f.Stat()
		if err != nil {
			return err
		}
		if !before.IsDir() {
			return fmt.Errorf("verification directory changed during capture")
		}
		fingerprints = append(fingerprints, fingerprint{dir, before})
		records = append(records, contentRecord{path: dir, mode: before.Mode()})
		for {
			batch, readErr := f.ReadDir(256)
			for _, entry := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries++
				if entries > MaxEntries {
					return fmt.Errorf("verification workspace exceeds %d entries; exclude dependency/output directories explicitly", MaxEntries)
				}
				path := entry.Name()
				if dir != "." {
					path = dir + "/" + path
				}
				pathBytes += len(path)
				if pathBytes > MaxPathBytes {
					return fmt.Errorf("verification workspace exceeds path byte limit")
				}
				if skip(path) {
					continue
				}
				info, err := handle.Lstat(filepath.FromSlash(path))
				if err != nil {
					return err
				}
				if info.IsDir() {
					if err := walk(path, depth+1); err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("verification input %q is not a regular file; exclude it explicitly", path)
				}
				input, err := handle.OpenFile(filepath.FromSlash(path), os.O_RDONLY|nonblock, 0)
				if err != nil {
					return err
				}
				before, err := input.Stat()
				if err != nil {
					input.Close()
					return err
				}
				if !before.Mode().IsRegular() || !os.SameFile(info, before) {
					input.Close()
					return fmt.Errorf("verification input %q changed during capture", path)
				}
				if before.Size() > MaxFileBytes || result.Bytes+before.Size() > MaxBytes {
					input.Close()
					return fmt.Errorf("verification input exceeds byte limits; exclude large dependencies/artifacts explicitly")
				}
				hash := sha256.New()
				n, readErr := io.Copy(hash, io.LimitReader(input, MaxFileBytes+1))
				after, statErr := input.Stat()
				closeErr := input.Close()
				if readErr != nil {
					return readErr
				}
				if statErr != nil {
					return statErr
				}
				if closeErr != nil {
					return closeErr
				}
				if n > MaxFileBytes || result.Bytes+n > MaxBytes {
					return fmt.Errorf("verification input exceeds byte limits")
				}
				if n != before.Size() || !sameFingerprint(before, after) {
					return fmt.Errorf("verification input %q changed during capture", path)
				}
				var sum [32]byte
				copy(sum[:], hash.Sum(nil))
				records = append(records, contentRecord{path, before.Mode(), sum})
				fingerprints = append(fingerprints, fingerprint{path, before})
				result.Files++
				result.Bytes += n
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return Snapshot{}, err
	}
	// Verify metadata again after the whole traversal to catch edits/removals and
	// directory-entry changes that occurred after an individual file was hashed.
	for _, entry := range fingerprints {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		now, err := handle.Lstat(filepath.FromSlash(entry.path))
		if err != nil {
			return Snapshot{}, err
		}
		if !sameFingerprint(entry.info, now) {
			return Snapshot{}, fmt.Errorf("verification workspace changed during capture")
		}
	}
	current, err := workspace.Identify(identity.Path)
	if err != nil {
		return Snapshot{}, err
	}
	if !identity.Matches(current) {
		return Snapshot{}, fmt.Errorf("verification workspace directory was replaced during capture")
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	hash := sha256.New()
	// JSON encoding is length-delimited and unambiguous for arbitrary filenames.
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(struct {
		Root    workspace.Identity
		Exclude []string
	}{identity, excludes}); err != nil {
		return Snapshot{}, err
	}
	for _, record := range records {
		if err := encoder.Encode(struct {
			Path string
			Mode os.FileMode
			Sum  string
		}{record.path, record.mode, hex.EncodeToString(record.sum[:])}); err != nil {
			return Snapshot{}, err
		}
	}
	result.Version = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func sameFingerprint(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
