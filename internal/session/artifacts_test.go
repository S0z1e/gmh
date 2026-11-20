package session

import (
	"encoding/json"
	"errors"
	"minimalharness/internal/llm"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func artifactFixture(t *testing.T, id string) (*Store, []string) {
	t.Helper()
	s, err := Open(id, OpenOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(llm.Message{Role: "user", Content: "history"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(map[string]string{"kind": "fixture"}); err != nil {
		t.Fatal(err)
	}
	paths := []string{s.Path, s.AuditPath()}
	for _, name := range []string{id + "-pre-compact-1", id + "-pre-rollback-2", id + "-custom-archive"} {
		p, err := s.ArchiveCopy(name)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for _, p := range []string{s.Path, paths[2], paths[3]} {
		backup := p + "-torn-ABCDEF2.bak"
		if err := os.WriteFile(backup, []byte("torn"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, backup)
	}
	tr := llm.RequestTrace{ID: "attempt", Usage: &llm.Usage{TotalTokens: 5}}
	if err := s.RecordUsage(UsageAttempt{ID: tr.ID, Request: tr}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.OutputDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.OutputDir(), "bash-fixture.log"), []byte("stdout"), 0600); err != nil {
		t.Fatal(err)
	}
	return s, paths
}

func TestArtifactDeleteIncludesFullFamily(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, paths := artifactFixture(t, "family")
	root := filepath.Dir(s.OutputDir())
	s.Close()
	if err := DeleteSession("family"); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(paths, root) {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("artifact left behind: %s %v", p, err)
		}
	}
}

func TestArtifactRenamePreservesArchivePaths(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, paths := artifactFixture(t, "family")
	root := filepath.Dir(s.OutputDir())
	if err := s.Rename("renamed"); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s.OutputDir()) != root {
		t.Fatal("stable output identity changed")
	}
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != len(paths)+1 {
		t.Fatalf("manifest=%+v", m)
	}
	for _, a := range m.Artifacts {
		if _, err := os.Lstat(filepath.Join(Dir(), a.Path)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths[2:5] {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rename invalidated a saved archive path: %v", err)
		}
	}
	s.Close()
	if err := DeleteSession("renamed"); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths[2:] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("left %s", path)
		}
	}
}

func TestArtifactRecoveryAtEveryRenameBoundary(t *testing.T) {
	for step := 0; step <= 9; step++ {
		t.Run(string(rune('A'+step)), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			s, _ := artifactFixture(t, "before")
			workspace := s.header.Workspace.Path
			m, err := s.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			op, err := prepareOperation("rename", "before", "after", m)
			if err != nil {
				t.Fatal(err)
			}
			n := min(step, len(op.Moves))
			s.Close()
			for _, move := range op.Moves[:n] {
				if err := reconcileMove(move, true); err != nil {
					t.Fatal(err)
				}
			}
			id := "before"
			if n == len(op.Moves) {
				id = "after"
			}
			reopened, err := Open(id, OpenOptions{Workspace: workspace, Existing: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range got.Artifacts {
				if _, err := os.Stat(filepath.Join(Dir(), a.Path)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(operationPath(op)); !os.IsNotExist(err) {
				t.Fatalf("operation not reconciled: %v", err)
			}
		})
	}
}

func TestArtifactRecoveryAtEveryDeleteBoundary(t *testing.T) {
	for step := 0; step <= 10; step++ {
		t.Run(string(rune('A'+step)), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			s, _ := artifactFixture(t, "before")
			workspace := s.header.Workspace.Path
			m, err := s.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			op, err := prepareOperation("delete", "before", "", m)
			if err != nil {
				t.Fatal(err)
			}
			n := min(step, len(op.Moves))
			s.Close()
			for _, move := range op.Moves[:n] {
				if err := reconcileMove(move, true); err != nil {
					t.Fatal(err)
				}
			}
			if n == len(op.Moves) {
				if err := DeleteSession("before"); err != nil {
					t.Fatal(err)
				}
			} else {
				reopened, err := Open("before", OpenOptions{Workspace: workspace})
				if err != nil {
					t.Fatal(err)
				}
				reopened.Close()
				if err := DeleteSession("before"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(operationPath(op)); !os.IsNotExist(err) {
				t.Fatalf("pending operation: %v", err)
			}
		})
	}
}

func TestArtifactSharedOutputAndArchiveNameIsolation(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, _ := artifactFixture(t, "source")
	root := filepath.Dir(s.OutputDir())
	workspace := s.header.Workspace.Path
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(Dir(), "copy-audit.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	copy, err := Open("copy-audit", OpenOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession("source"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("active shared output not protected: %v", err)
	}
	copy.Close()
	legacyArchive := filepath.Join(Dir(), "source-pre-compact-legacy.jsonl")
	if err := os.WriteFile(legacyArchive, data, 0600); err != nil {
		t.Fatal(err)
	}
	other, err := Open("source-pre-rollback-99", OpenOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("shared output removed: %v", err)
	}
	if _, err := os.Stat(other.Path); err != nil {
		t.Fatalf("independent session removed: %v", err)
	}
	if err := DeleteSession("copy-audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyArchive); !os.IsNotExist(err) {
		t.Fatalf("last owner left an unregistered archive: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("final owner output remains: %v", err)
	}
}

func TestArtifactCustomArchiveIsNotListedAsSession(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ArchiveCopy("arbitrary-name"); err != nil {
		t.Fatal(err)
	}
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "source" {
		t.Fatalf("archive listed as session: %+v", list)
	}
}
func TestArtifactIndependentAuditNameIsNotDeleted(t *testing.T) {
	setTestHome(t, t.TempDir())
	a, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := New("source-audit")
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if err := DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("source-audit"); err != nil {
		t.Fatal("independent transcript deleted", err)
	}
}
func TestArtifactMetadataRejectsSymlinkFamilies(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Dir(s.OutputDir())); err != nil {
		t.Skip(err)
	}
	if _, err := s.ArchiveCopy("archive"); err == nil {
		t.Fatal("registered archive in aliased family")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside metadata written: %+v", entries)
	}
}

func TestArtifactLateAuditCannotResurrectDeletedSession(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("gone")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := DeleteSession("gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(map[string]string{"private": "late"}); err == nil {
		t.Fatal("late audit recreated session content")
	}
	if _, err := os.Stat(s.AuditPath()); !os.IsNotExist(err) {
		t.Fatalf("audit resurrected: %v", err)
	}
	replacement, err := New("gone")
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
	if err := s.Audit(map[string]string{"private": "old owner"}); err == nil {
		t.Fatal("late audit accepted a replacement owner")
	}
}

func TestArtifactRecoveryRejectsInvalidPlanWithoutMutation(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, _ := artifactFixture(t, "source")
	defer s.Close()
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("rename", "source", "target", m)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupOperation(op)
	for _, mutate := range []func(*artifactOperation){
		func(op *artifactOperation) { op.Moves[0].Kind = "unknown" },
		func(op *artifactOperation) { op.Moves[0].To = "unrelated.jsonl" },
		func(op *artifactOperation) { op.Moves[1] = op.Moves[0] },
	} {
		changed := op
		changed.Moves = append([]artifactMove(nil), op.Moves...)
		mutate(&changed)
		if err := validateOperation(changed); err == nil {
			t.Fatal("accepted damaged operation plan")
		}
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactKeepsLegacyArchiveAfterOutputAdoption(t *testing.T) {
	setTestHome(t, t.TempDir())
	root := t.TempDir()
	s, err := Open("legacy", OpenOptions{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	header := *s.header
	header.OutputID = ""
	line, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	archive := filepath.Join(Dir(), "legacy-pre-compact-1.jsonl")
	for _, path := range []string{s.Path, archive} {
		if err := os.WriteFile(path, append(line, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	backup := archive + "-torn-ABCDEF2.bak"
	if err := os.WriteFile(backup, []byte("torn"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open("legacy", OpenOptions{Workspace: root, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("renamed"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{archive, backup} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rename lost old archive path %s: %v", path, err)
		}
	}
	s.Close()
	if err := DeleteSession("renamed"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{archive, backup} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("delete missed %s: %v", path, err)
		}
	}
}

func TestArtifactPendingRollbackRejectsLateAudit(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, _ := artifactFixture(t, "before")
	root := s.header.Workspace.Path
	manifest, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("rename", "before", "after", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.auditFile.Close(); err != nil {
		t.Fatal(err)
	}
	s.auditFile = nil
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	s.file = nil // Same frozen state as a failed rollback in Rename.
	for _, move := range op.Moves {
		if move.Kind == "audit" {
			if err := reconcileMove(move, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Audit(map[string]string{"kind": "late"}); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("audit wrote into pending rename: %v", err)
	}
	s.Close()
	reopened, err := Open("before", OpenOptions{Workspace: root, Existing: true})
	if err != nil {
		t.Fatalf("late audit prevented recovery: %v", err)
	}
	reopened.Close()
}

func TestArtifactUnboundOwnerLeavesBoundRollbackName(t *testing.T) {
	setTestHome(t, t.TempDir())
	other, err := New("source-pre-rollback-99")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	path := filepath.Join(Dir(), "source.jsonl")
	if err := os.WriteFile(path, []byte("{\"role\":\"user\",\"content\":\"legacy\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other.Path); err != nil {
		t.Fatalf("independent session was removed: %v", err)
	}
}

func TestArtifactActiveRenameRecoversDestinationOperation(t *testing.T) {
	setTestHome(t, t.TempDir())
	dst, _ := artifactFixture(t, "destination")
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	root := source.header.Workspace.Path
	m, err := dst.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("delete", "destination", "", m)
	if err != nil {
		t.Fatal(err)
	}
	dst.Close()
	for _, move := range op.Moves {
		if err := reconcileMove(move, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Rename("destination"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(operationPath(op)); !os.IsNotExist(err) {
		t.Fatalf("rename left a stale destination journal: %v", err)
	}
	source.Close()
	reopened, err := Open("destination", OpenOptions{Workspace: root, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func TestArtifactResumePromotesArchiveToWritableSession(t *testing.T) {
	for _, name := range []string{"recoverable", "original-pre-rollback-1"} {
		t.Run(name, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			root := t.TempDir()
			original, err := Open("original", OpenOptions{Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := original.ArchiveCopy(name); err != nil {
				t.Fatal(err)
			}
			original.Close()
			resumed, err := Open(name, OpenOptions{Workspace: root, Existing: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := resumed.Append(llm.Message{Role: "user", Content: "new work"}); err != nil {
				t.Fatal(err)
			}
			resumed.Close()
			list, err := List()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(list, func(info Info) bool { return info.ID == name }) {
				t.Fatalf("resumed session missing from list: %+v", list)
			}
			if err := DeleteSession("original"); err != nil {
				t.Fatal(err)
			}
			messages, err := Load(name)
			if err != nil || len(messages) != 1 || messages[0].Content != "new work" {
				t.Fatalf("original owner's deletion affected new work: %+v %v", messages, err)
			}
			if err := RenameSession(name, "recovered"); err != nil {
				t.Fatal(err)
			}
			if err := DeleteSession("recovered"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArtifactCopyRecoversDestinationOperation(t *testing.T) {
	setTestHome(t, t.TempDir())
	dst, _ := artifactFixture(t, "destination")
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	m, err := dst.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("delete", "destination", "", m)
	if err != nil {
		t.Fatal(err)
	}
	dst.Close()
	for _, move := range op.Moves {
		if err := reconcileMove(move, true); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := source.ArchiveCopy("destination")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(operationPath(op)); !os.IsNotExist(err) {
		t.Fatalf("archive poisoned destination recovery: %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactSharedResumeRecoversFamilyBeforeUpdatingRegistry(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, _ := artifactFixture(t, "original")
	workspace := s.header.Workspace.Path
	output := filepath.Dir(s.OutputDir())
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "copy.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("rename", "original", "renamed", m)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, move := range op.Moves {
		if err := reconcileMove(move, true); err != nil {
			t.Fatal(err)
		}
	}
	copy, err := Open("copy", OpenOptions{Workspace: workspace, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := copy.ArchiveCopy("copy-pre-rollback-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := copy.Rename("copy-renamed"); err != nil {
		t.Fatal(err)
	}
	copy.Close()
	renamed, err := Open("renamed", OpenOptions{Workspace: workspace, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	renamed.Close()
	for _, id := range []string{"renamed", "copy-renamed"} {
		if err := DeleteSession(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{archive, output} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("final deletion missed %s: %v", path, err)
		}
	}
}

func TestArtifactRenameRecoversAuditDestination(t *testing.T) {
	setTestHome(t, t.TempDir())
	dest, err := New("target-audit")
	if err != nil {
		t.Fatal(err)
	}
	source, _ := artifactFixture(t, "source")
	defer source.Close()
	leaveCommittedDelete(t, dest)

	if err := source.Rename("target"); err != nil {
		t.Fatal(err)
	}
	source.Close()
	if _, err := List(); err != nil {
		t.Fatalf("rename left an unrecoverable audit destination: %v", err)
	}
}

func TestArtifactCannotChainRenameWhilePriorPlanRemains(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, _ := artifactFixture(t, "before")
	workspace := s.header.Workspace.Path
	leaveCommittedRename(t, s, "after")
	if err := s.Rename("third"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("rename advanced beyond a still-pending plan: %v", err)
	}
	s.Close()
	s, err := Open("after", OpenOptions{Workspace: workspace, Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Rename("third"); err != nil {
		t.Fatalf("rename still blocked after journal recovery: %v", err)
	}
}

func TestArtifactRenameDoesNotAcceptUnrelatedDeleteRecovery(t *testing.T) {
	setTestHome(t, t.TempDir())
	dest, err := New("destination")
	if err != nil {
		t.Fatal(err)
	}
	dest.Close()
	source, err := New("deleted")
	if err != nil {
		t.Fatal(err)
	}
	leaveCommittedDelete(t, source)
	if err := RenameSession("deleted", "destination"); err == nil {
		t.Fatal("rename treated a recovered delete as a successful move")
	}
}

func leaveCommittedDelete(t *testing.T, s *Store) {
	t.Helper()
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("delete", s.ID, "", m)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, move := range op.Moves {
		if err := reconcileMove(move, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArtifactAutoIDSkipsReservedRenameSource(t *testing.T) {
	setTestHome(t, t.TempDir())
	const base = "20261120-090000"
	s, _ := artifactFixture(t, base)
	defer s.Close()
	leaveCommittedRename(t, s, "target")

	unlock, err := lockManagement()
	if err != nil {
		t.Fatal(err)
	}
	generated, err := createAutoStore(Dir(), base, nil)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer generated.Close()
	if generated.ID != base+"-1" {
		t.Fatalf("generated ID reused reserved source: %s", generated.ID)
	}
	s.Close()
	if _, err := List(); err != nil {
		t.Fatalf("generated ID prevented earlier recovery: %v", err)
	}
}

func leaveCommittedRename(t *testing.T, s *Store, to string) {
	t.Helper()
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	op, err := prepareOperation("rename", s.ID, to, m)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(operationPath(op))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(operationPath(op)); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(to); err != nil {
		t.Fatal(err)
	}
	// Recreate the committed plan as if cleanup had failed after the rename.
	if err := os.WriteFile(operationPath(op), journal, 0600); err != nil {
		t.Fatal(err)
	}
}
