//go:build !windows

package main

import (
	"os"
	"syscall"
)

const tuiBinaryName = "harness-tui"

func runTUIProcess(path string, args []string) error {
	return syscall.Exec(path, args, os.Environ())
}
