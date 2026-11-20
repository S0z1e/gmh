//go:build !windows

package verification

import "syscall"

const nonblock = syscall.O_NONBLOCK
