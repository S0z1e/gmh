package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCaseAliasRevokesOriginalDecision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	parent := t.TempDir()
	root := filepath.Join(parent, "Workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "workspace")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("case-sensitive filesystem")
	}
	id, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	aliased, err := Identify(alias)
	if err != nil {
		t.Fatal(err)
	}
	if !sameIdentity(id, aliased) || trustName(id) != trustName(aliased) {
		t.Fatalf("case alias has different identity: %+v vs %+v", id, aliased)
	}
	if err := Decide(id, "trust"); err != nil {
		t.Fatal(err)
	}
	if err := Decide(aliased, "untrust"); err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(id); err != nil || trusted {
		t.Fatalf("case alias failed to revoke: %v %v", trusted, err)
	}
}

func TestOversizedTrustRecordFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id, err := Identify(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Decide(id, "trust"); err != nil {
		t.Fatal(err)
	}
	dir, _ := trustDir()
	path := filepath.Join(dir, trustName(id))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, bytes.Repeat([]byte(" "), (64<<10)-len(data))...)
	data = append(data, []byte("INVALID")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(id); trusted || err == nil {
		t.Fatalf("oversized corrupt record accepted: %v %v", trusted, err)
	}
}

func TestNormalizationDoesNotMergeDistinctDirectories(t *testing.T) {
	parent := t.TempDir()
	a := filepath.Join(parent, "Project")
	b := filepath.Join(parent, "project")
	if err := os.Mkdir(a, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0700); os.IsExist(err) {
		t.Skip("case-insensitive filesystem")
	} else if err != nil {
		t.Fatal(err)
	}
	first, err := Identify(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Identify(b)
	if err != nil {
		t.Fatal(err)
	}
	if sameIdentity(first, second) || trustName(first) == trustName(second) {
		t.Fatalf("distinct directories merged: %+v %+v", first, second)
	}
}

func TestTrustIdentityHandlesVolumeAndUnicodeAliases(t *testing.T) {
	for _, pair := range [][2]string{
		{`\\Server\Share\Repo`, `\\SERVER\SHARE\repo`},
		{"/projects/caf\u00e9", "/PROJECTS/cafe\u0301"},
	} {
		a := Identity{Path: pair[0], FileID: "same-incarnation"}
		b := Identity{Path: pair[1], FileID: a.FileID}
		if !sameIdentity(a, b) || trustName(a) != trustName(b) {
			t.Fatalf("aliases differ: %+v %+v", a, b)
		}
		b.FileID = "different-incarnation"
		if sameIdentity(a, b) || trustName(a) == trustName(b) {
			t.Fatal("different incarnations share trust")
		}
	}
}

func TestTrustDoesNotSurviveDeleteAndRecreate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Decide(id, "trust"); err != nil {
		t.Fatal(err)
	}
	// Ordinary directory-content changes must not invalidate trust.
	p := filepath.Join(root, "file")
	if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	changed, err := Identify(root)
	if err != nil || !sameIdentity(changed, id) {
		t.Fatalf("content change invalidated identity: %+v %v", changed, err)
	}
	for i := 0; i < 128; i++ {
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		replacement, err := Identify(root)
		if err != nil {
			t.Fatal(err)
		}
		if trusted, err := Trusted(replacement); err != nil || trusted {
			t.Fatalf("replacement %d inherited trust: %+v %v", i, replacement, err)
		}
	}
}

func TestIdentifyThroughSearchOnlyAncestor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ancestor := t.TempDir()
	root := filepath.Join(ancestor, "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0711); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ancestor, 0700)
	id, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Decide(id, "trust"); err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(id); err != nil || !trusted {
		t.Fatalf("accessible workspace unusable: %v %v", trusted, err)
	}
}

func TestMissingIncarnationNeverReusesTrust(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := Identity{Path: t.TempDir()}
	if trusted, err := Trusted(id); trusted || err != nil {
		t.Fatalf("missing incarnation trusted: %v %v", trusted, err)
	}
	if err := Decide(id, "trust"); err == nil {
		t.Fatal("persisted trust without incarnation")
	}
}

func TestIdentifySearchOnlyWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0711); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if _, err := Identify(root); err != nil {
		t.Fatal(err)
	}
}

func TestTrustIsExplicitPersistentAndRevocable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	id, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(id); err != nil || trusted {
		t.Fatalf("new directory trusted: %v %v", trusted, err)
	}
	if err := Decide(id, "trust"); err != nil {
		t.Fatal(err)
	}
	again, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(again); err != nil || !trusted {
		t.Fatalf("decision lost: %v %v", trusted, err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err == nil {
		aliased, err := Identify(alias)
		if err != nil || aliased != id {
			t.Fatalf("symlink identity: %+v %v", aliased, err)
		}
	}
	if err := Decide(id, "untrust"); err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(id); err != nil || trusted {
		t.Fatalf("revocation lost: %v %v", trusted, err)
	}
}

func TestTrustDoesNotTransferToReplacementOrChildDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Decide(id, "trust"); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	childID, err := Identify(child)
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(childID); err != nil || trusted {
		t.Fatalf("child inherited trust: %v %v", trusted, err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	replacement, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := Trusted(replacement); err != nil || trusted {
		t.Fatalf("replacement inherited trust: %v %v", trusted, err)
	}
}

func TestConcurrentTrustDecisionsForDifferentWorkspaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var ids []Identity
	for i := 0; i < 10; i++ {
		id, err := Identify(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Decide(id, "trust"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if trusted, err := Trusted(id); err != nil || !trusted {
			t.Fatalf("concurrent decision lost: %v %v", trusted, err)
		}
	}
}
