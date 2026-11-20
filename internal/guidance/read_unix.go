//go:build !windows

package guidance

import "syscall"

// Opening a swapped FIFO must not block the agent before f.Stat can reject it.
const nonblockRead = syscall.O_NONBLOCK
