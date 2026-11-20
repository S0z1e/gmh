package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"minimalharness/internal/llm"
	"os"
	"path/filepath"
	"time"
)

const artifactRetention = 7 * 24 * time.Hour
const maxRetentionScanBytes = 512 << 20

type PruneReport struct {
	DeletedBlobs   int   `json:"deleted_blobs"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
}

// PruneArtifacts explicitly reclaims old image blobs only after a complete
// reference scan. Archives, cleared history and recovery records all retain
// their references. Unknown/corrupt/incomplete history disables reclamation.
// Session deletion handles the owned output family; delivered output receipts
// separately govern the seven-day capture retention policy.
func PruneArtifacts(ctx context.Context, now time.Time) (PruneReport, error) {
	var report PruneReport
	unlock, err := lockManagement()
	if err != nil {
		return report, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if _, err := recoverOperations(); err != nil {
		return report, err
	}
	if dir, err := metadataDirectory(".operations", false); err == nil {
		f, err := os.Open(dir)
		if err != nil {
			return report, err
		}
		entries, err := f.ReadDir(maxSessionArtifacts + 1)
		f.Close()
		if err != nil && err != io.EOF {
			return report, err
		}
		if len(entries) > maxSessionArtifacts {
			return report, fmt.Errorf("too many pending artifact operations")
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".json" {
				return report, fmt.Errorf("artifact operation pending; retry pruning after its writer closes")
			}
		}
	} else if !os.IsNotExist(err) {
		return report, err
	}
	entries, err := artifactEntries()
	if err != nil {
		return report, err
	}
	refs := map[string]bool{}
	var scanned int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		name := entry.Name()
		// Recovery fragments may contain a reference that never reached a complete
		// record. Refuse an uncertain scan rather than guessing what it contained.
		if filepath.Ext(name) != ".jsonl" && filepath.Ext(name) != ".bak" {
			continue
		}
		path := filepath.Join(Dir(), name)
		if err := checkTranscriptPath(path); err != nil {
			return report, err
		}
		f, err := os.OpenFile(path, os.O_RDONLY|metadataReadFlags, 0)
		if err != nil {
			return report, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return report, err
		}
		if !st.Mode().IsRegular() || st.Size() > maxRetentionScanBytes-scanned {
			f.Close()
			return report, fmt.Errorf("artifact reference scan exceeds size/type limit")
		}
		remaining := int64(maxRetentionScanBytes) - scanned
		var visitErr error
		end, _, torn, scanErr := scanRecords(io.LimitReader(f, remaining+1), func(line []byte, _ llm.Message) {
			if visitErr != nil {
				return
			}
			if err := ctx.Err(); err != nil {
				visitErr = err
				return
			}
			dec := json.NewDecoder(bytes.NewReader(line))
			for {
				tok, err := dec.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					visitErr = err
					break
				}
				if digest, ok := tok.(string); ok && validImageDigest(digest) {
					refs[digest] = true
					if len(refs) > maxArtifactScan {
						visitErr = fmt.Errorf("too many image references")
						break
					}
				}
			}
		})
		f.Close()
		scanned += end
		if scanErr != nil {
			return report, scanErr
		}
		if visitErr != nil {
			return report, visitErr
		}
		if torn || scanned > maxRetentionScanBytes {
			return report, fmt.Errorf("artifact reference scan incomplete")
		}
	}
	base, err := os.OpenRoot(filepath.Dir(Dir()))
	if err != nil {
		return report, err
	}
	defer base.Close()
	for _, name := range []string{"blobs", filepath.Join("blobs", "sha256")} {
		st, err := base.Lstat(name)
		if os.IsNotExist(err) {
			return report, nil
		}
		if err != nil {
			return report, err
		}
		if !st.IsDir() {
			return report, fmt.Errorf("image blob directory must not be an alias")
		}
	}
	root, err := base.OpenRoot(filepath.Join("blobs", "sha256"))
	if err != nil {
		return report, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return report, err
	}
	defer dir.Close()
	// Collect and bound candidates before the first deletion.
	candidates := map[string]os.FileInfo{}
	count := 0
	for {
		batch, err := dir.ReadDir(256)
		count += len(batch)
		if count > maxArtifactScan {
			return report, fmt.Errorf("too many image blobs")
		}
		for _, entry := range batch {
			if !validImageDigest(entry.Name()) || refs[entry.Name()] {
				continue
			}
			st, err := entry.Info()
			if err != nil {
				return report, err
			}
			if st.Mode().IsRegular() && !st.ModTime().After(now.Add(-artifactRetention)) {
				candidates[entry.Name()] = st
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return report, err
		}
	}
	for name, before := range candidates {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		st, err := root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return report, err
		}
		if !st.Mode().IsRegular() || !os.SameFile(st, before) || st.ModTime() != before.ModTime() {
			continue
		}
		if err := root.Remove(name); err != nil {
			return report, err
		}
		report.DeletedBlobs++
		report.ReclaimedBytes += st.Size()
	}
	if report.DeletedBlobs > 0 {
		if err := syncRecoveryDirectory(imageBlobDir()); err != nil {
			return report, err
		}
	}

	return report, nil
}
