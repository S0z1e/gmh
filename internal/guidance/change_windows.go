package guidance

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_BASIC_INFO includes NTFS/ReFS metadata ChangeTime; Win32FileAttributeData
// returned by os.Stat carries only creation/access/write time.
func changeTime(f *os.File, _ os.FileInfo) (int64, error) {
	var info struct {
		CreationTime   int64
		LastAccessTime int64
		LastWriteTime  int64
		ChangeTime     int64
		Attributes     uint32
		Padding        uint32
	}
	err := windows.GetFileInformationByHandleEx(windows.Handle(f.Fd()), windows.FileBasicInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return info.ChangeTime, err
}
