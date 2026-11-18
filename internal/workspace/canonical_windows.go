package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	volumeNameDOS  = 0
	volumeNameGUID = 1
	fileNameOpened = 8
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
	if err == nil {
		if legacy == abs {
			return Identity{Path: abs, FileID: id}, nil
		}
		if legacyID, err := directoryID(legacy); err == nil && legacyID == id {
			return Identity{Path: legacy, FileID: id}, nil
		}
	}
	physical, nameErr := resolvedDirectoryName(h)
	if nameErr != nil {
		if err != nil {
			// The filesystem cannot supply a canonical name; the caller's
			// exact path still identifies the directory we opened.
			return Identity{Path: abs, FileID: id}, nil
		}
		return Identity{}, &os.PathError{Op: "resolve directory", Path: abs, Err: nameErr}
	}
	namespace := bindingNamespace(abs)
	if strings.HasPrefix(strings.ToLower(filepath.VolumeName(namespace)), `\\?\volume{`) {
		// A drive-letter volume can also be opened by GUID. Compare the
		// handle in the caller's namespace before replacing that binding.
		if guidPath, err := finalDirectoryPath(h, volumeNameGUID); err == nil && sameDirectBindingName(namespace, guidPath) {
			return Identity{Path: abs, FileID: id}, nil
		}
	}
	return Identity{Path: fallbackBindingPath(abs, physical, func(path string) bool {
		// Open the ordinary spelling as-is: the metadata helper adds a
		// device prefix, so it cannot tell if Win32 accepts the plain name.
		check, err := openDirectoryPath(path, windows.FILE_READ_ATTRIBUTES, 0)
		if err != nil {
			return false
		}
		defer windows.CloseHandle(check)
		plainID, err := directoryHandleID(check, path)
		return err == nil && plainID == id
	}), FileID: id}, nil
}

func fallbackBindingPath(requested, physical string, ordinaryUsable func(string) bool) string {
	canonical := windowsCanonicalSpelling(physical)
	// Preserve the requested namespace only if the name already spells the
	// physical location. When legacy resolution fails on a symlink alias,
	// the handle's physical name must instead match opening its target.
	if sameDirectBindingName(bindingNamespace(requested), bindingNamespace(canonical)) {
		return requested
	}
	// A long name may be usable without a prefix when Win32 long paths are
	// enabled. Keep the prefix only if the ordinary name cannot open it.
	if units, err := windows.UTF16FromString(canonical); err == nil && len(units) > windows.MAX_PATH && canonical != physical && !ordinaryUsable(canonical) {
		return physical
	}
	return canonical
}

// Case-folded path strings can compare equal even when one of their components
// is a link in a case-sensitive directory. For each case mismatch, inspect the
// requested component without following its final reparse point.
func sameDirectBindingName(requested, physical string) bool {
	if !strings.EqualFold(requested, physical) {
		return false
	}
	for requested != physical && requested != filepath.Dir(requested) {
		if filepath.Base(requested) != filepath.Base(physical) {
			h, err := openDirectoryPath(requested, 0, windows.FILE_FLAG_OPEN_REPARSE_POINT)
			if err != nil {
				return false
			}
			var info windows.ByHandleFileInformation
			err = windows.GetFileInformationByHandle(h, &info)
			windows.CloseHandle(h)
			if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return false
			}
		}
		requested, physical = filepath.Dir(requested), filepath.Dir(physical)
	}
	return true
}

func bindingNamespace(path string) string {
	path = windowsMetadataPath(path)
	if strings.HasPrefix(path, `\\.\`) {
		return `\\?\` + path[4:]
	}
	return path
}

func resolvedDirectoryName(h windows.Handle) (string, error) {
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
