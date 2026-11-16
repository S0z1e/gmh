//go:build !windows

package session

import "syscall"

// A corrupted/racing FIFO must not block hydration before f.Stat can reject it.
const imageBlobReadFlags = syscall.O_NONBLOCK
