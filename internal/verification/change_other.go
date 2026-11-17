//go:build !darwin && !linux && !windows

package verification

import (
	"fmt"
	"os"
)

func changeTime(*os.File, os.FileInfo) (int64, error) {
	return 0, fmt.Errorf("verification change time is unsupported on this platform")
}
