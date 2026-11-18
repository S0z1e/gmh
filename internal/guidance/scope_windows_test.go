package guidance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"minimalharness/internal/workspace"
)

func TestWindowsExtendedScopePath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("USERPROFILE", base)
	if strings.HasPrefix(base, `\\`) {
		t.Skip("fixture requires a local drive")
	}
	root := filepath.Join(base, strings.Repeat("one", 40), strings.Repeat("two", 40))
	put(t, filepath.Join(base, "AGENTS.md"), "home guidance")
	put(t, filepath.Join(root, "AGENTS.md"), "extended path guidance")
	extended := `\\?\` + root
	bound, err := openCanonicalScope(extended)
	if err != nil {
		t.Fatal(err)
	}
	bound.Close()
	snap, err := Resolve(context.Background(), extended, filepath.Join(extended, "new.go"))
	if err != nil || len(snap.Files) != 2 || snap.Files[0].Prefix != "home guidance" || snap.Files[1].Prefix != "extended path guidance" {
		t.Fatalf("extended workspace: %+v, %v", snap, err)
	}
	f := snap.Files[1]
	chunk, err := Read(context.Background(), extended, snap.Target, f.Path, f.SHA256, 0, 128)
	if err != nil || chunk.Text != f.Prefix {
		t.Fatalf("extended continuation: %+v, %v", chunk, err)
	}
}

func TestWindowsGuidanceWithRestrictedACL(t *testing.T) {
	for _, tc := range []struct {
		name, mask string
		ancestor   bool
		access     uint32
	}{
		{"readable scope without explicit traverse", "0x20", false, windows.FILE_TRAVERSE},
		{"searchable ancestor without listing or metadata", "0x81", true, windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("USERPROFILE", t.TempDir())
			parent := t.TempDir()
			root := filepath.Join(parent, "repo")
			put(t, filepath.Join(root, "AGENTS.md"), "restricted ACL guidance")
			protected := root
			if tc.ancestor {
				protected = parent
			}
			extended := root
			if !strings.HasPrefix(root, `\\`) {
				extended = `\\?\` + root
			}
			prior, err := workspace.Identify(extended)
			if err != nil {
				t.Fatal(err)
			}
			if err := workspace.Decide(prior, "trust"); err != nil {
				t.Fatal(err)
			}
			denyWindowsDirectoryAccess(t, protected, tc.mask, tc.access)
			after, err := workspace.Identify(extended)
			if err != nil || !prior.Matches(after) {
				t.Fatalf("ACL change lost the same workspace binding: was %+v, now %+v, %v", prior, after, err)
			}
			if trusted, err := workspace.Trusted(after); err != nil || !trusted {
				t.Fatalf("saved trust decision lost after ACL change: %t, %v", trusted, err)
			}
			snap, err := Resolve(context.Background(), root, "new.go")
			if err != nil || len(snap.Files) != 1 || snap.Files[0].Prefix != "restricted ACL guidance" {
				t.Fatalf("resolve restricted workspace: %+v, %v", snap, err)
			}
			identity, err := workspace.Identify(root)
			if err != nil || identity.Path != snap.Root || identity.FileID == "" {
				t.Fatalf("identify restricted workspace: %+v, %v", identity, err)
			}
			f := snap.Files[0]
			chunk, err := Read(context.Background(), root, snap.Target, f.Path, f.SHA256, 0, 128)
			if err != nil || chunk.Text != f.Prefix {
				t.Fatalf("read restricted workspace: %+v, %v", chunk, err)
			}
		})
	}
}

func denyWindowsDirectoryAccess(t *testing.T, path, mask string, access uint32) {
	t.Helper()
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	old, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		t.Skip("filesystem does not support ACLs")
	}
	if err != nil {
		t.Fatal(err)
	}
	oldACL, _, err := old.DACL()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := old.Control()
	if err != nil {
		t.Fatal(err)
	}
	sid := owner.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(D;;%s;;;%s)(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)", mask, sid, sid))
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	setACL := func(acl *windows.ACL, flags windows.SECURITY_INFORMATION) error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|flags, nil, nil, acl, nil)
	}
	if err := setACL(acl, windows.PROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		flags := windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
		if control&windows.SE_DACL_PROTECTED != 0 {
			flags = windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
		if err := setACL(oldACL, flags); err != nil {
			t.Errorf("restore directory ACL: %v", err)
		}
	})
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		windows.CloseHandle(h)
		t.Fatal("test directory unexpectedly granted access denied by its ACL")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("test directory access should be denied: %v", err)
	}
}

func TestWindowsExtendedOnlyWorkspaceContinuesHomeGuidance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	if strings.HasPrefix(home, `\\`) {
		t.Skip("fixture requires a local drive")
	}
	root := `\\?\` + filepath.Join(home, "extended-only.")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Skipf("filesystem does not accept extended directory names: %v", err)
	}
	text := strings.Repeat("a", PrefixBytes) + "actual end"
	put(t, filepath.Join(home, "AGENTS.md"), text)
	s, err := Resolve(context.Background(), root, "new.go")
	if err != nil || len(s.Files) != 1 || !s.Files[0].More {
		t.Fatalf("home guidance not available: %+v, %v", s, err)
	}
	f := s.Files[0]
	chunk, err := Read(context.Background(), root, s.Target, f.Path, f.SHA256, len(f.Prefix), 128)
	if err != nil || chunk.Text != "actual end" || chunk.More {
		t.Fatalf("cannot continue advertised home guidance: %+v, %v", chunk, err)
	}
}
