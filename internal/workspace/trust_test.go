package workspace

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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
