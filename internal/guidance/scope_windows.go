package guidance

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openCanonicalScope(path string) (*os.Root, error) {
	volumeRoot := filepath.VolumeName(path) + string(filepath.Separator)
	rel, err := filepath.Rel(volumeRoot, path)
	if err != nil {
		return nil, err
	}
	rootName, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return nil, err
	}
	const metadata = windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	const traverse = windows.FILE_TRAVERSE | windows.SYNCHRONIZE
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	access := uint32(metadata)
	if rel != "." {
		access = traverse
	}
	handle, err := windows.CreateFile(rootName, access, share, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open guidance scope", Path: path, Err: err}
	}
	if rel != "." {
		// Traverse without FILE_GENERIC_READ: a parent may allow traversal
		// but deny other rights such as reading the access control list.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i, part := range parts {
			access = traverse
			if i == len(parts)-1 {
				// This handle is for identity only. A readable directory
				// need not grant FILE_TRAVERSE explicitly to its caller.
				access = metadata
			}
			name, err := windows.NewNTUnicodeString(part)
			if err != nil {
				windows.CloseHandle(handle)
				return nil, err
			}
			attrs := windows.OBJECT_ATTRIBUTES{
				RootDirectory: handle,
				ObjectName:    name,
				Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
			}
			attrs.Length = uint32(unsafe.Sizeof(attrs))
			var next windows.Handle
			var status windows.IO_STATUS_BLOCK
			err = windows.NtCreateFile(&next, access, &attrs, &status, nil, 0, share,
				windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
			windows.CloseHandle(handle)
			if err != nil {
				return nil, &os.PathError{Op: "open guidance scope", Path: path, Err: err}
			}
			handle = next
		}
	}
	return openVerifiedScope(path, os.NewFile(uintptr(handle), path))
}

// ancestrySpelling compares DOS/UNC volumes across equivalent namespace names.
// Keep the individual components unmodified; a trailing dot or space can be
// significant in an extended path. This is only a containment comparison.
func ancestrySpelling(path string) string {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		name := path[4:]
		if len(name) >= 4 && strings.EqualFold(name[:4], `UNC\`) {
			return `\\` + name[4:]
		}
		if len(name) >= 3 && name[1] == ':' && name[2] == '\\' {
			return name
		}
	}
	return path
}
