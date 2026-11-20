package guidance

import (
	"fmt"
	"os"
	"syscall"
)

func changeTime(_ *os.File, info os.FileInfo) (int64, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("instruction source change time unavailable")
	}
	return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec, nil
}
