//go:build !windows

package fileguard

import "syscall"

const nonblockingRead = syscall.O_NONBLOCK
