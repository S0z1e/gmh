package tools

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

const bashShellCompletionPolicy = "once the shell exits, remaining processes in its group are stopped after a bounded output drain."

// waitBashExit leaves the shell waitable, reserving its PID until group cleanup.
func waitBashExit(cmd *exec.Cmd) (bool, error) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil, err
	}
}

func stopBashProcessGroup(cmd *exec.Cmd) error { return killProcessTree(cmd) }
