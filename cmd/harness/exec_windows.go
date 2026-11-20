package main

import (
	"errors"
	"os"
	"os/exec"
)

const tuiBinaryName = "harness-tui.exe"

func runTUIProcess(path string, args []string) error {
	cmd := exec.Command(path, args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			os.Exit(exited.ExitCode())
		}
		os.Exit(1)
	}
	return nil
}
