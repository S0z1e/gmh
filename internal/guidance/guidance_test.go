package guidance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScopeOrderTargetAndRefresh(t *testing.T) {
	home, err := canonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	root := filepath.Join(home, "repo")
	paths := []string{filepath.Join(home, "AGENTS.md"), filepath.Join(root, "AGENTS.md"), filepath.Join(root, "CLAUDE.md"), filepath.Join(root, "pkg", "AGENTS.md"), filepath.Join(root, "pkg", "deep", "AGENTS.md")}
	for _, path := range paths {
		put(t, path, path)
	}
	put(t, filepath.Join(root, "sibling", "AGENTS.md"), "unrelated instructions")
	target := filepath.Join("pkg", "deep", "missing", "new.go")
	snap, err := Resolve(context.Background(), root, target)
	if err != nil || len(snap.Files) != len(paths) {
		t.Fatalf("scope=%+v err=%v", snap, err)
	}
	for i, file := range snap.Files {
		if file.Path != paths[i] || file.SHA256 != digest([]byte(paths[i])) {
			t.Fatalf("wrong source order/identity: %+v", snap)
		}
	}
	again, err := Resolve(context.Background(), root, target)
	if err != nil || again.SHA256 != snap.SHA256 {
		t.Fatal("unchanged scope not stable")
	}
	put(t, paths[3], "updated instructions")
	changed, err := Resolve(context.Background(), root, target)
	if err != nil || changed.SHA256 == snap.SHA256 || changed.Files[3].SHA256 == snap.Files[3].SHA256 {
		t.Fatal("mid-session edit missed")
	}
	other, err := Resolve(context.Background(), root, "sibling/new.go")
	if err != nil || len(other.Files) != 4 || other.Files[3].Prefix != "unrelated instructions" {
		t.Fatalf("target scope did not switch: %+v %v", other, err)
	}
	if _, err := Resolve(context.Background(), root, "../outside.go"); err == nil {
		t.Fatal("escaped target accepted")
	}
	if err := os.Remove(paths[3]); err != nil {
		t.Fatal(err)
	}
	removed, err := Resolve(context.Background(), root, target)
	if err != nil || len(removed.Files) != len(paths)-1 || removed.SHA256 == changed.SHA256 {
		t.Fatal("source removal was not observed")
	}
}

func TestLongSourceCanBeReadInBoundedExactChunks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "repo")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "AGENTS.md")
	text := strings.Repeat("中🙂\n", 1500) + "TAIL instruction"
	put(t, path, text)
	snap, err := Resolve(context.Background(), root, "new.go")
	if err != nil || len(snap.Files) != 1 {
		t.Fatalf("%+v %v", snap, err)
	}
	source := snap.Files[0]
	if !source.More || !utf8.ValidString(source.Prefix) || len(source.Prefix) > PrefixBytes || source.SHA256 != digest([]byte(text)) {
		t.Fatalf("invalid long source: %+v", source)
	}
	assembled := source.Prefix
	offset := len(assembled)
	for offset < len(text) {
		chunk, err := Read(context.Background(), root, "new.go", path, source.SHA256, offset, 1024)
		if err != nil || chunk.Next <= offset || !utf8.ValidString(chunk.Text) || len(chunk.Text) > 1024 {
			t.Fatalf("bad chunk %+v %v", chunk, err)
		}
		assembled += chunk.Text
		offset = chunk.Next
	}
	if assembled != text {
		t.Fatal("chunked source changed original bytes")
	}
	if _, err := Read(context.Background(), root, "new.go", path, source.SHA256, 1, 1024); err == nil {
		t.Fatal("split UTF8 boundary accepted")
	}
	if _, err := Read(context.Background(), root, "new.go", path, source.SHA256, 0, PrefixBytes+1); err == nil {
		t.Fatal("unbounded chunk accepted")
	}
	put(t, path, strings.Repeat("x", len(text)))
	if _, err := Read(context.Background(), root, "new.go", path, source.SHA256, offset, 1024); err == nil {
		t.Fatal("stale hash accepted")
	}
}

func TestReadCannotAccessUnrelatedSources(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	a := filepath.Join(root, "a", "AGENTS.md")
	b := filepath.Join(root, "b", "AGENTS.md")
	put(t, a, "a rules")
	put(t, b, "b rules")
	if _, err := Read(context.Background(), root, "a/new.go", b, digest([]byte("b rules")), 0, 128); err == nil {
		t.Fatal("sibling instruction source leaked into target scope")
	}
	secret := filepath.Join(filepath.Dir(root), "private.txt")
	put(t, secret, "secret")
	if _, err := Read(context.Background(), root, "a/new.go", secret, digest([]byte("secret")), 0, 128); err == nil {
		t.Fatal("read became an arbitrary file exception")
	}
	if _, err := Read(context.Background(), root, "a/new.go", a, "", 0, 128); err == nil {
		t.Fatal("missing source identity accepted")
	}
}

func TestSourceBoundsAndCancellationFailExplicitly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(root, "AGENTS.md")
	put(t, path, strings.Repeat("x", MaxSourceBytes+1))
	if _, err := Resolve(context.Background(), root, "new.go"); err == nil {
		t.Fatal("oversized instructions silently clipped")
	}
	put(t, path, string([]byte{0xff}))
	if _, err := Resolve(context.Background(), root, "new.go"); err == nil {
		t.Fatal("invalid source bytes repaired")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Resolve(ctx, "/does-not-exist", "new.go"); err != context.Canceled {
		t.Fatalf("cancelled resolve did IO: %v", err)
	}
	put(t, path, "rules")
	deep := strings.Repeat("sub/", MaxDirectories+1) + "file.go"
	if _, err := Resolve(context.Background(), root, deep); err == nil {
		t.Fatal("deep scope silently omitted instructions")
	}
}

func TestSymlinkTargetUsesEffectiveDirectoryAndRejectsEscapes(t *testing.T) {
	root, err := canonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	put(t, filepath.Join(root, "real", "AGENTS.md"), "real rules")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skip(err)
	}
	snap, err := Resolve(context.Background(), root, "alias/new.go")
	if err != nil || len(snap.Files) != 1 || snap.Files[0].Scope != filepath.Join(root, "real") || snap.Target != filepath.Join(root, "real", "new.go") {
		t.Fatalf("alias target scope incorrect: %+v %v", snap, err)
	}
	outside := t.TempDir()
	put(t, filepath.Join(outside, "AGENTS.md"), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), root, "escape/new.go"); err == nil {
		t.Fatal("target escaped workspace")
	}
	if err := os.Symlink(filepath.Join(outside, "AGENTS.md"), filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), root, "real/new.go"); err == nil {
		t.Fatal("instruction source escaped its directory")
	}
}

func TestRootAliasAndAbsoluteTargetRetainSourceIdentity(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	put(t, filepath.Join(root, "pkg", "AGENTS.md"), "rules")
	if err := os.Symlink("real", alias); err != nil {
		t.Skip(err)
	}
	actual, err := Resolve(context.Background(), root, "pkg/new.go")
	if err != nil {
		t.Fatal(err)
	}
	linked, err := Resolve(context.Background(), alias, filepath.Join(alias, "pkg", "new.go"))
	if err != nil || actual.SHA256 != linked.SHA256 {
		t.Fatalf("alias changed effective scope: %+v %v", linked, err)
	}
}

func TestAggregateSourceLimitDoesNotSilentlyDropInstructions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	dir := root
	text := strings.Repeat("x", MaxSourceBytes)
	for i := 0; i < 9; i++ {
		put(t, filepath.Join(dir, "AGENTS.md"), text)
		dir = filepath.Join(dir, "sub")
	}
	if _, err := Resolve(context.Background(), root, filepath.Join(dir, "new.go")); err == nil {
		t.Fatal("aggregate source budget was not enforced")
	}
}

func TestDanglingInstructionSourceMustNotDisappear(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.Symlink("missing-guide", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skip(err)
	}
	if _, err := Resolve(context.Background(), root, "new.go"); err == nil {
		t.Fatal("existing but broken instruction source silently disappeared")
	}
}

type scopeSwapContext struct {
	context.Context
	calls int
	swap  func()
}

func (c *scopeSwapContext) Err() error {
	c.calls++
	if c.calls == 4 {
		c.swap()
	}
	return nil
}
func TestInstructionParentSwapCannotEscapeWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	put(t, filepath.Join(outside, "AGENTS.md"), "OUTSIDE SECRET")
	ctx := &scopeSwapContext{Context: context.Background(), swap: func() {
		if err := os.Rename(sub, sub+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, sub); err != nil {
			t.Fatal(err)
		}
	}}
	snapshot, err := Resolve(ctx, root, "sub/new.go")
	if err == nil {
		t.Fatalf("reopened scoped source outside workspace: %+v", snapshot)
	}
}

func TestCanonicalRootTargetCanBeReusedThroughAlias(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	put(t, filepath.Join(root, "AGENTS.md"), "rules")
	if err := os.Symlink("real", alias); err != nil {
		t.Skip(err)
	}
	snap, err := Resolve(context.Background(), alias, "")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := Resolve(context.Background(), alias, snap.Target)
	if err != nil || snap.SHA256 != replay.SHA256 {
		t.Fatalf("canonical root target not reusable: %+v %v", replay, err)
	}
	if _, err := Read(context.Background(), alias, snap.Target, snap.Files[0].Path, snap.Files[0].SHA256, 0, 128); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidPathBytesCannotAliasManifestIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if _, err := Resolve(context.Background(), root, "sub\xff/new.go"); err == nil {
		t.Fatal("non-UTF8 target identity accepted")
	}
	if _, err := Read(context.Background(), root, "new.go", "bad\xff/AGENTS.md", "", 0, 128); err == nil {
		t.Fatal("non-UTF8 source identity accepted")
	}
}

func TestRelativeTargetUsesMutationPathNormalization(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	put(t, filepath.Join(root, "AGENTS.md"), "root rules")
	put(t, filepath.Join(root, "pkg", "AGENTS.md"), "nested rules")
	if err := os.Mkdir(filepath.Join(root, "pkg", "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("pkg", "sub"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	sep := string(filepath.Separator)
	rel := "alias" + sep + ".." + sep + "new.go"
	relative, err := Resolve(context.Background(), root, rel)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := Resolve(context.Background(), root, root+sep+rel)
	if err != nil {
		t.Fatal(err)
	}
	if relative.SHA256 != absolute.SHA256 || len(relative.Files) != 1 || relative.Files[0].Prefix != "root rules" {
		t.Fatalf("relative target scope = %s, want %s with just root rules", relative.Target, absolute.Target)
	}
}

func TestRelativeTargetCanLexicallyReturnToWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	t.Setenv("HOME", t.TempDir())
	put(t, filepath.Join(root, "AGENTS.md"), "root rules")
	sep := string(filepath.Separator)
	target := strings.Join([]string{"alias", "..", "..", "repo", "new.go"}, sep)
	relative, err := Resolve(context.Background(), root, target)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := Resolve(context.Background(), root, filepath.Join(root, "new.go"))
	if err != nil || relative.SHA256 != absolute.SHA256 {
		t.Fatalf("normalizing %q yielded %+v; want %+v (err=%v)", target, relative, absolute, err)
	}
}

func TestCanonicalScopesRejectAncestorReplacement(t *testing.T) {
	cases := []struct {
		name        string
		parent      bool
		alreadyOpen bool
	}{
		{name: "outer scope before opening"},
		{name: "parent of outer scope before opening", parent: true},
		{name: "ancestor after pinning", parent: true, alreadyOpen: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			outer := filepath.Join(home, "parent", "outer")
			root := outer
			for i := 1; i < MaxAncestorLevels; i++ {
				root = filepath.Join(root, "nested")
			}
			put(t, filepath.Join(outer, "AGENTS.md"), "outer guidance")
			put(t, filepath.Join(root, "AGENTS.md"), "workspace guidance")
			canonical, err := canonicalRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			var roots *sourceRoots
			if tc.alreadyOpen {
				roots, err = openSourceRoots(canonical)
				if err != nil {
					t.Fatal(err)
				}
				defer roots.close()
			}
			replaced := outer
			if tc.parent {
				replaced = filepath.Dir(outer)
			}
			moved := filepath.Join(t.TempDir(), "moved")
			if err := os.Rename(replaced, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, replaced); err != nil {
				t.Skipf("cannot replace ancestor with symlink: %v", err)
			}
			if !tc.alreadyOpen {
				roots, err = openSourceRoots(canonical)
				if err != nil {
					return
				}
				defer roots.close()
				t.Fatal("opened a scope through a substituted ancestor symlink")
			}
			if err := roots.check(); err == nil {
				t.Fatal("published pinned scopes after canonical ancestry became a symlink")
			}
		})
	}
}

func TestVerifiedScopeRejectsDifferentDirectory(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	pinned, err := os.Open(oldDir)
	if err != nil {
		t.Fatal(err)
	}
	if handle, err := openVerifiedScope(newDir, pinned); err == nil {
		handle.Close()
		t.Fatal("scope reopen accepted a different directory from the pinned identity")
	}
}
