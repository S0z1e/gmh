package workspace

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const trustReadFlags = 0

func directoryID(path string) (string, error) {
	handle, err := openDirectoryMetadata(path)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	return directoryHandleID(handle, path)
}

func openDirectoryMetadata(path string) (windows.Handle, error) {
	return openDirectoryPath(windowsMetadataPath(path))
}

func openDirectoryPath(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// Query metadata without requesting FILE_LIST_DIRECTORY. A path may
	// be known and traversable even when its parent can't be enumerated.
	return windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// DirectoryFileID identifies a pinned directory handle without reopening its path.
func DirectoryFileID(f *os.File) (string, error) {
	return directoryHandleID(windows.Handle(f.Fd()), f.Name())
}

func directoryHandleID(handle windows.Handle, path string) (string, error) {
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
