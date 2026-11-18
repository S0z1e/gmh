//go:build !linux && !darwin

package tools

import "os/exec"

const bashShellCompletionPolicy = "starting it with & lets the shell exit and closes output capture shortly afterward."

// These platforms retain their existing post-Wait capture drain. Group cleanup
// after normal exit requires a platform-specific way to retain process ownership.
func waitBashExit(cmd *exec.Cmd) (bool, error) { return false, nil }

func stopBashProcessGroup(cmd *exec.Cmd) error { return killProcessTree(cmd) }
