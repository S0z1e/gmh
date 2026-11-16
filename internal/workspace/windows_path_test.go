package workspace

import "testing"

func TestWindowsMetadataPathPreservesDeviceNamespaces(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{`C:\repo`, `\\?\C:\repo`},
		{`\\server\share\repo`, `\\?\UNC\server\share\repo`},
		{`\\.\C:\repo`, `\\.\C:\repo`},
		{`\\.\UNC\server\share\repo`, `\\.\UNC\server\share\repo`},
		{`\\?\C:\repo`, `\\?\C:\repo`},
		{`\\?\UNC\server\share\repo`, `\\?\UNC\server\share\repo`},
		{`\\?\Volume{00000000-0000-0000-0000-000000000000}\repo`, `\\?\Volume{00000000-0000-0000-0000-000000000000}\repo`},
	} {
		if got := windowsMetadataPath(tc.path); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.path, got, tc.want)
		}
	}
}
