package workspace

import (
	"fmt"
	"os"
	"syscall"
)

const trustReadFlags = 0

func directoryID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory: %s", path)
	}
	var stat syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", stat.VolumeSerialNumber, stat.FileIndexHigh, stat.FileIndexLow, stat.CreationTime.HighDateTime, stat.CreationTime.LowDateTime), nil
}

func syncDirectory(string) error { return nil }
