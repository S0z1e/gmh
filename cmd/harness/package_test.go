package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// CI supplies a native extracted release. A copy of this test executable runs
// beside the real packaged TUI and uses exactly the production lookup/launch.
func TestPackagedSibling(t *testing.T) {
	child := os.Getenv("GMH_PACKAGE_CHILD")
	if child == "1" || child == "exit" {
		sibling, err := tuiSibling()
		if err != nil {
			t.Fatal(err)
		}
		arg := "-h"
		if child == "exit" {
			arg = "-package-probe-invalid-flag"
		}
		if err := runTUIProcess(sibling, []string{sibling, arg}); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := os.Getenv("HARNESS_PACKAGED_DIR")
	if dir == "" {
		t.Skip("set HARNESS_PACKAGED_DIR to an extracted native release")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	name := "sibling-probe"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	probe := filepath.Join(dir, name)
	if err := os.WriteFile(probe, b, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probe, "-test.run=^TestPackagedSibling$")
	cmd.Env = append(os.Environ(), "GMH_PACKAGE_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native sibling launch: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, probe, "-test.run=^TestPackagedSibling$")
	cmd.Env = append(os.Environ(), "GMH_PACKAGE_CHILD=exit")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("child exit failure was lost: %s", out)
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("TUI exit was not preserved: %v %s", err, out)
		}
	}
	for _, binary := range []string{"harness", "harness-tui"} {
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		cmd := exec.CommandContext(ctx, filepath.Join(dir, binary), "-h")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s help: %v\n%s", binary, err, out)
		}
	}
}
