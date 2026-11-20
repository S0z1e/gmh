//go:build !darwin && !linux && !windows

package guidance

import (
	"fmt"
	"os"
)

func changeTime(*os.File, os.FileInfo) (int64, error) {
	return 0, fmt.Errorf("instruction source change time is unsupported on this platform")
}
