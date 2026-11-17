package workdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveMatchesBashCwdSpelling(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ cwd, want string }{
		{"", root}, {"  ", root}, {"sub/..", root}, {filepath.Join(root, "sub"), filepath.Join(root, "sub")},
		{root + string(filepath.Separator) + "link" + string(filepath.Separator) + "..", root},
	} {
		if got := Resolve(root, tc.cwd); got != tc.want {
			t.Fatalf("Resolve(%q)=%q want %q", tc.cwd, got, tc.want)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/deep", filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	implicit := root + string(filepath.Separator) + "link" + string(filepath.Separator) + ".."
	canonicalRoot, err := Canonical(root, implicit)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalRoot != want {
		t.Fatalf("explicit cwd resolved through symlink: %s want %s", canonicalRoot, want)
	}
	implicitRoot, err := Canonical(implicit, "")
	if err != nil {
		t.Fatal(err)
	}
	if implicitRoot != filepath.Join(want, "sub") {
		t.Fatalf("implicit root was lexically cleaned: %s", implicitRoot)
	}
}
