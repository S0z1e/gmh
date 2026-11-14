package main

import (
	"os"
	"os/exec"
)

const tuiBinaryName = "harness-tui.exe"

func runTUIProcess(path string, args []string) error {
	cmd := exec.Command(path, args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
