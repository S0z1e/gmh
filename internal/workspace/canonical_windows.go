package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// CanonicalDirectory resolves links through a directory handle. Unlike
// filepath.EvalSymlinks' Windows case normalization, this does not enumerate
// the caller's directory path one component at a time.
func CanonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	h, err := openDirectoryMetadata(abs)
	if err != nil {
		return "", &os.PathError{Op: "open directory metadata", Path: abs, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", &os.PathError{Op: "stat directory", Path: abs, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	const volumeNameDOS, fileNameOpened = 0, 8
	resolved, err := finalDirectoryPath(h, volumeNameDOS)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		// Some SMB servers cannot normalize a handle without checking
		// access to each component. Ask for the opened name instead.
		resolved, err = finalDirectoryPath(h, volumeNameDOS|fileNameOpened)
	}
	if err != nil {
		return "", &os.PathError{Op: "resolve directory", Path: abs, Err: err}
	}
	return windowsCanonicalSpelling(resolved, abs), nil
}

func finalDirectoryPath(h windows.Handle, flags uint32) (string, error) {
	buf := make([]uint16, 256)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// Preserve the input namespace for saved workspace bindings. For ordinary
// DOS/UNC inputs remove Win32's handle prefix only when reopening the name
// through the ordinary namespace would still refer to the same path.
func windowsCanonicalSpelling(path, input string) string {
	const extended = `\\?\`
	const device = `\\.\`
	if strings.HasPrefix(input, extended) {
		return path
	}
	if strings.HasPrefix(input, device) && strings.HasPrefix(path, extended) {
		return device + path[len(extended):]
	}
	if !strings.HasPrefix(path, extended) {
		return path
	}
	name := path[len(extended):]
	var ordinary string
	if len(name) >= 4 && strings.EqualFold(name[:4], `UNC\`) {
		ordinary = `\\` + name[4:]
	} else if len(name) >= 3 && name[1] == ':' && name[2] == '\\' {
		ordinary = name
	} else {
		return path
	}
	rel := strings.TrimPrefix(ordinary[len(filepath.VolumeName(ordinary)):], `\`)
	if rel == "" {
		return ordinary
	}
	for _, component := range strings.Split(rel, `\`) {
		if !filepath.IsLocal(component) || strings.TrimRight(component, ". ") != component {
			return path
		}
	}
	return ordinary
}
