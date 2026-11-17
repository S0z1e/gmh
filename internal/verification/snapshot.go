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
	MaxEntries    = 50000
	MaxBytes      = 128 << 20
	MaxFileBytes  = 16 << 20
	MaxPathBytes  = 8 << 20
	MaxExclusions = 1024
	maxDepth      = 256
)

type Snapshot struct {
	Root    workspace.Identity `json:"root"`
	Version string             `json:"version"`
	Files   int                `json:"files"`
	Bytes   int64              `json:"bytes"`
}

type fingerprint struct {
	path   string
	info   os.FileInfo
	change int64
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
	if len(exclude) > MaxExclusions {
		return result, fmt.Errorf("verification exceeds %d exclusions", MaxExclusions)
	}
	excludes := []string{".git"}
	exclusionBytes := 0
	for _, path := range exclude {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		exclusionBytes += len(path)
		if exclusionBytes > MaxPathBytes {
			return result, fmt.Errorf("verification exclusions exceed path byte limit")
		}
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
	handle, err := openSnapshotRoot(identity)
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
		change, err := changeTime(f, before)
		if err != nil {
			return err
		}
		fingerprints = append(fingerprints, fingerprint{dir, before, change})
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
				change, err := changeTime(input, before)
				if err != nil {
					input.Close()
					return err
				}
				hash := sha256.New()
				n, readErr := io.Copy(hash, io.LimitReader(input, MaxFileBytes+1))
				after, statErr := input.Stat()
				var afterChange int64
				if statErr == nil {
					afterChange, statErr = changeTime(input, after)
				}
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
				if n != before.Size() || change != afterChange || !sameFingerprint(before, after) {
					return fmt.Errorf("verification input %q changed during capture", path)
				}
				var sum [32]byte
				copy(sum[:], hash.Sum(nil))
				records = append(records, contentRecord{path, before.Mode(), sum})
				fingerprints = append(fingerprints, fingerprint{path, before, change})
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
		current, err := handle.OpenFile(filepath.FromSlash(entry.path), os.O_RDONLY|nonblock, 0)
		if err != nil {
			return Snapshot{}, err
		}
		currentInfo, err := current.Stat()
		if err != nil {
			current.Close()
			return Snapshot{}, err
		}
		changed, err := changeTime(current, currentInfo)
		closeErr := current.Close()
		if err != nil {
			return Snapshot{}, err
		}
		if closeErr != nil {
			return Snapshot{}, closeErr
		}
		if changed != entry.change || !sameFingerprint(entry.info, currentInfo) {
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
	// JSON byte slices use base64, preserving arbitrary filename bytes losslessly.
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(struct {
		RootPath []byte
		RootID   string
		Exclude  [][]byte
	}{[]byte(identity.Path), identity.FileID, bytePaths(excludes)}); err != nil {
		return Snapshot{}, err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if err := encoder.Encode(struct {
			Path []byte
			Mode os.FileMode
			Sum  string
		}{[]byte(record.path), record.mode, hex.EncodeToString(record.sum[:])}); err != nil {
			return Snapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	result.Version = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func sameFingerprint(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func bytePaths(paths []string) [][]byte {
	result := make([][]byte, len(paths))
	for i, path := range paths {
		result[i] = []byte(path)
	}
	return result
}
