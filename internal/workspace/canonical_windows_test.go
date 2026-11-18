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
