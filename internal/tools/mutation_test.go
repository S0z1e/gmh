package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPinnedMutationCannotFollowLateAliasSwap(t *testing.T) {
	for _, directory := range []bool{false, true} {
		root := t.TempDir()
		for _, dir := range []string{"public-dir", ".git"} {
			if err := os.Mkdir(filepath.Join(root, dir), 0755); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{"public-dir/config", ".git/config"} {
			if err := os.WriteFile(filepath.Join(root, path), []byte("KEEP"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		path, initial, replacement := "alias", "public-dir/config", ".git/config"
		if directory {
			path, initial, replacement = "alias/config", "public-dir", ".git"
		}
		if err := os.Symlink(initial, filepath.Join(root, "alias")); err != nil {
			t.Skip(err)
		}
		e := &Executor{Root: root}
		m, err := e.PrepareMutation(path)
		if err != nil {
			t.Fatal(err)
		}
		if m.Protected {
			t.Fatal("ordinary target marked protected")
		}
		if err := m.target.Check(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "alias")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacement, filepath.Join(root, "alias")); err != nil {
			t.Fatal(err)
		}
		// Model a swap in the gap after Check: the commit never follows it.
		if err := atomicWriteTarget(m.target, []byte("UPDATED")); err != nil {
			t.Fatal(err)
		}
		m.Close()
		data, err := os.ReadFile(filepath.Join(root, ".git/config"))
		if err != nil || string(data) != "KEEP" {
			t.Fatalf("late swap reached protected target: %q %v", data, err)
		}
		data, err = os.ReadFile(filepath.Join(root, "public-dir/config"))
		if err != nil || string(data) != "UPDATED" {
			t.Fatalf("pinned target lost: %q %v", data, err)
		}
	}
}

func TestProtectedDirectoryAliasesEndingInDots(t *testing.T) {
	for _, tc := range []struct{ protected, link, path string }{
		{".git", ".", "file"}, {".git", "real/", "real/file"},
		{".git", "real/.", "real/file"}, {".git", "real/sub/..", "real/file"},
		{".agents/plugins", "../..", "file"},
	} {
		t.Run(tc.protected+tc.link, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"real/sub", ".agents"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(tc.link, filepath.Join(root, tc.protected)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			e := &Executor{Root: root}
			m, err := e.PrepareMutation(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if !m.Protected || !IsProtectedWorkspacePath(root, tc.path) {
				t.Fatal("directory alias classified as unprotected")
			}
		})
	}
}

func TestUnresolvableProtectedRootsRequireApproval(t *testing.T) {
	for _, link := range []string{".harness.json", "absolute"} {
		t.Run(link, func(t *testing.T) {
			root := t.TempDir()
			target := link
			if link == "absolute" {
				target = filepath.Join(root, "actual")
			}
			if err := os.Symlink(target, filepath.Join(root, ".harness.json")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			e := &Executor{Root: root}
			m, err := e.PrepareMutation("actual")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if !m.Protected || !IsProtectedWorkspacePath(root, "actual") {
				t.Fatal("unresolved protected root was treated as safe")
			}
		})
	}
}

func TestPreparedMutationIsBoundToRequestedPath(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	m, err := e.PrepareMutation("public")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = e.Dispatch(WithMutation(context.Background(), m), "write", `{"path":".harness.json","content":"bad"}`)
	if err == nil {
		t.Fatal("approved lease reused for another path")
	}
	if _, err := os.Stat(filepath.Join(e.Root, ".harness.json")); !os.IsNotExist(err) {
		t.Fatalf("unapproved target created: %v", err)
	}
}

func TestNewMutationParentCannotBecomeASymlink(t *testing.T) {
	e := &Executor{Root: t.TempDir()}
	if err := os.Mkdir(filepath.Join(e.Root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	m, err := e.PrepareMutation("new/config")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := os.Symlink(".git", filepath.Join(e.Root, "new")); err != nil {
		t.Skip(err)
	}
	_, err = e.Dispatch(WithMutation(context.Background(), m), "write", `{"path":"new/config","content":"bad"}`)
	if err == nil || !strings.Contains(err.Error(), "target changed") {
		t.Fatalf("new parent not invalidated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, ".git/config")); !os.IsNotExist(err) {
		t.Fatalf("unapproved target created: %v", err)
	}
}

func TestConcurrentAliasSwapsCannotBypassProtectedMutationPolicy(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"public", ".git"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "config"), []byte("KEEP"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("public", filepath.Join(root, "alias")); err != nil {
		t.Skip(err)
	}
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	var swaps atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			target := "public"
			if i%2 == 0 {
				target = ".git"
			}
			tmp := filepath.Join(root, "next-link")
			if err := os.Symlink(target, tmp); err != nil {
				errCh <- err
				return
			}
			if err := os.Rename(tmp, filepath.Join(root, "alias")); err != nil {
				os.Remove(tmp)
				errCh <- err
				return
			}
			swaps.Add(1)
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	e := &Executor{Root: root}
	for i := 0; i < 300; i++ {
		m, err := e.PrepareMutation("alias/config")
		if err != nil {
			continue
		}
		if !m.Protected {
			_, _ = e.Dispatch(WithMutation(context.Background(), m), "write", `{"path":"alias/config","content":"CHANGED","overwrite":true}`)
		}
		m.Close()
	}
	data, err := os.ReadFile(filepath.Join(root, ".git/config"))
	if err != nil || string(data) != "KEEP" {
		t.Fatalf("concurrent swaps bypassed protection: %q %v", data, err)
	}
	select {
	case err := <-errCh:
		t.Skipf("atomic symlink replacement unavailable: %v", err)
	default:
	}
	if swaps.Load() == 0 {
		t.Fatal("race fixture did not swap any aliases")
	}
}
