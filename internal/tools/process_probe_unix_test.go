//go:build !windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// procDead reports whether the process is gone, or a zombie (dead,
// unreaped — Kill(pid, 0) still succeeds on a zombie).
func procDead(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS != "linux" {
		state, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil {
			return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		}
		return strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	// State is the field after the closing paren of the comm
	// (the comm itself may contain ')' and spaces).
	if i := strings.LastIndexByte(string(data), ')'); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}
