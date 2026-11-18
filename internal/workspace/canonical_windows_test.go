package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsBindingPreservesLegacyAliasPath(t *testing.T) {
	base := t.TempDir()
	if strings.HasPrefix(base, `\\`) {
		t.Skip("fixture requires a local drive")
	}
	target := filepath.Join(base, "workspace")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	want, err := Identify(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, aliasType := range []string{"junction", "extended symlink"} {
		t.Run(aliasType, func(t *testing.T) {
			alias := filepath.Join(base, aliasType)
			switch aliasType {
			case "junction":
				// Commands only interpolate the fixed fixture names; the base
				// path is passed separately as the working directory.
				cmd := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", "junction", "workspace")
				cmd.Dir = base
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Skipf("unable to create fixture junction: %v (%s)", err, out)
				}
			case "extended symlink":
				if err := os.Symlink(target, alias); err != nil {
					t.Skipf("unable to create fixture symlink: %v", err)
				}
				alias = `\\?\` + alias
			}
			// Trust records and session headers previously used exactly
			// this EvalSymlinks spelling and the target's directory ID.
			old, err := filepath.EvalSymlinks(alias)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Identify(alias)
			if err != nil || !got.Matches(Identity{Path: old, FileID: want.FileID}) {
				t.Fatalf("old binding %q / %q failed to reopen: %+v, %v", old, want.FileID, got, err)
			}
			physical, err := CanonicalDirectory(alias)
			if err != nil || !strings.EqualFold(physical, want.Path) {
				t.Fatalf("physical folder for guidance = %q, %v; want %q", physical, err, want.Path)
			}
		})
	}
}

func TestWindowsCanonicalDirectoryAcceptsVolumeGUID(t *testing.T) {
	root := t.TempDir()
	volume := filepath.VolumeName(root)
	if len(volume) != 2 || volume[1] != ':' {
		t.Skip("fixture requires a local drive")
	}
	mount, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		t.Fatal(err)
	}
	var buf [64]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(mount, &buf[0], uint32(len(buf))); err != nil {
		t.Skipf("cannot obtain local volume GUID: %v", err)
	}
	guid := windows.UTF16ToString(buf[:]) + strings.TrimPrefix(root[len(volume):], `\`)
	canonical, err := CanonicalDirectory(guid)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := os.Stat(canonical)
	if err != nil || !os.SameFile(original, resolved) {
		t.Fatalf("volume GUID %q resolved to %q (%v); want directory %q", guid, canonical, err, root)
	}
}

func TestWindowsBindingDoesNotLoseSignificantTrailingDot(t *testing.T) {
	base := t.TempDir()
	if strings.HasPrefix(base, `\\`) {
		t.Skip("fixture requires a local drive")
	}
	sibling := filepath.Join(base, "workspace")
	dotted := `\\?\` + sibling + "."
	if err := os.Mkdir(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dotted, 0700); err != nil {
		t.Skipf("filesystem does not accept extended directory names: %v", err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(dotted, alias); err != nil {
		t.Skipf("cannot create Windows symlink: %v", err)
	}
	want, err := directoryID(dotted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Identify(alias)
	if err != nil || got.FileID != want {
		t.Fatalf("alias rebound to a sibling: %+v, %v; expected ID %q", got, err, want)
	}
	reopened, err := directoryID(got.Path)
	if err != nil || reopened != want {
		t.Fatalf("identity pathname %q does not reopen the expected directory: %q, %v", got.Path, reopened, err)
	}
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	direct, err := Identify(dotted)
	if err != nil {
		t.Fatal(err)
	}
	viaAlias, err := Identify(alias)
	if err != nil || !direct.Matches(viaAlias) {
		t.Fatalf("alias without plain-named sibling lost its canonical binding: direct=%+v alias=%+v err=%v", direct, viaAlias, err)
	}
}

func TestFallbackBindingPathKeepsOnlyDirectNamespace(t *testing.T) {
	long := `\\?\C:\` + strings.Repeat(`long-directory\`, 20) + "workspace"
	for _, tc := range []struct {
		request, physical, want string
		ordinaryUsable          bool
	}{
		{`\\?\C:\work\repo`, `\\?\C:\work\repo`, `\\?\C:\work\repo`, true},
		{`\\.\C:\work\repo`, `\\?\C:\work\repo`, `\\.\C:\work\repo`, true},
		{`C:\work\alias`, `\\?\C:\work\repo.`, `\\?\C:\work\repo.`, false},
		{`\\?\C:\work\alias`, `\\?\D:\repos\repo`, `D:\repos\repo`, true},
		{`C:\work\alias`, long, long, false},
		{`C:\work\alias`, long, long[4:], true},
		{long, long, long, true},
	} {
		if got := fallbackBindingPath(tc.request, tc.physical, func(string) bool { return tc.ordinaryUsable }); got != tc.want {
			t.Errorf("request %q, physical %q: got %q, want %q", tc.request, tc.physical, got, tc.want)
		}
	}
}

func TestWindowsCaseOnlyJunctionIsNotADirectBinding(t *testing.T) {
	parent := t.TempDir()
	if out, err := exec.Command("fsutil.exe", "file", "setCaseSensitiveInfo", parent, "enable").CombinedOutput(); err != nil {
		t.Skipf("cannot create case-sensitive NTFS fixture: %v (%s)", err, out)
	}
	physical := filepath.Join(parent, "repo")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", "Repo", "repo")
	cmd.Dir = parent
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot create case-only junction: %v (%s)", err, out)
	}
	alias := filepath.Join(parent, "Repo")
	if sameDirectBindingName(bindingNamespace(alias), bindingNamespace(physical)) {
		t.Fatal("case-only junction treated as direct open")
	}
}
