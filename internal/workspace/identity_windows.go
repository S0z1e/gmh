package workspace

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

const trustReadFlags = 0

func directoryID(path string) (string, error) {
	// Identify supplies an absolute path; preserve support for long paths when
	// opening directly through Win32 rather than os.Open's internal conversion.
	if !strings.HasPrefix(path, `\\?\`) {
		if strings.HasPrefix(path, `\\`) {
			path = `\\?\UNC\` + path[2:]
		} else {
			path = `\\?\` + path
		}
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	// Query metadata without GENERIC_READ / FILE_LIST_DIRECTORY. Traversable
	// directories may permit attributes while denying directory enumeration.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var stat windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &stat); err != nil {
		return "", err
	}
	if stat.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", fmt.Errorf("workspace root is not a directory: %s", path)
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", stat.VolumeSerialNumber, stat.FileIndexHigh, stat.FileIndexLow, stat.CreationTime.HighDateTime, stat.CreationTime.LowDateTime), nil
}

func syncDirectory(string) error { return nil }
