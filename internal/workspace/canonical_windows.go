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
	resolved, err := resolvedDirectoryName(h)
	if err != nil {
		return "", &os.PathError{Op: "resolve directory", Path: abs, Err: err}
	}
	return windowsCanonicalSpelling(resolved), nil
}

// Keep the established binding spelling only when it resolves to the same
// directory as the requested path. Go may lose significant Windows namespace
// information while following a link, even when the rewritten name exists.
func identify(path string) (Identity, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Identity{}, err
	}
	h, err := openDirectoryMetadata(abs)
	if err != nil {
		return Identity{}, &os.PathError{Op: "open directory metadata", Path: abs, Err: err}
	}
	defer windows.CloseHandle(h)
	id, err := directoryHandleID(h, abs)
	if err != nil {
		return Identity{}, err
	}
	legacy, err := filepath.EvalSymlinks(abs)
	switch {
	case err != nil:
		// The known path can still be opened after a parent stops allowing
		// enumeration. Keep its namespace (including any extended prefix).
		return Identity{Path: abs, FileID: id}, nil
	case legacy == abs:
		return Identity{Path: abs, FileID: id}, nil
	default:
		if legacyID, err := directoryID(legacy); err == nil && legacyID == id {
			return Identity{Path: legacy, FileID: id}, nil
		}
	}
	actual, err := resolvedDirectoryName(h)
	if err != nil {
		return Identity{}, &os.PathError{Op: "resolve directory", Path: abs, Err: err}
	}
	return Identity{Path: windowsCanonicalSpelling(actual), FileID: id}, nil
}

func resolvedDirectoryName(h windows.Handle) (string, error) {
	const volumeNameDOS, volumeNameGUID, fileNameOpened = 0, 1, 8
	path, err := finalDirectoryPath(h, volumeNameDOS)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		// Some SMB servers require permission to list every component in
		// order to normalize the name. The opened name does not need that.
		path, err = finalDirectoryPath(h, volumeNameDOS|fileNameOpened)
	}
	if errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		// A local volume can be mounted without any DOS drive letter.
		path, err = finalDirectoryPath(h, volumeNameGUID)
	}
	return path, err
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

// Use ordinary DOS/UNC path spellings when doing so cannot change which
// directory the name denotes. Volume GUIDs and extended-only names retain
// Win32's prefix.
func windowsCanonicalSpelling(path string) string {
	const extended = `\\?\`
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
