package tools

import (
	"fmt"
	"os/exec"
	"strings"
)

func procDead(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	return err == nil && !strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid))
}
