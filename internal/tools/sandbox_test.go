package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxArgs(t *testing.T) {
	root := "/tmp/ws/root"
	args := sandboxArgs(root)
	want := []string{
		"--tmpfs", "/",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/sbin", "/sbin",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", root, root,
		"--chdir", root,
		"--setenv", "HOME", root,
		"--unshare-user",
		"--unshare-ipc",
		"--die-with-parent",
	}
	if len(args) != len(want) {
		t.Fatalf("sandboxArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("sandboxArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

// fakeBwrap writes a pass-through bwrap: it logs its argv, drops
// everything up to "--", and execs the rest.
func writeFakeBwrap(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> \"$BWRAP_LOG\"\n" +
		"while [ \"$1\" != \"--\" ]; do shift; done\n" +
		"shift\n" +
		"exec \"$@\"\n"
	path := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBashSandboxedFakeBwrap(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(t.TempDir(), "bwrap.log")
	t.Setenv("BWRAP_LOG", log)
	e := &Executor{Root: root, Sandbox: true, BwrapPath: writeFakeBwrap(t)}
	out, err := e.Bash(context.Background(), BashArgs{Command: "echo hi"})
	if err != nil || out != "hi\n" {
		t.Fatalf("sandboxed bash = %q, err %v", out, err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(data)
	for _, want := range []string{"--tmpfs /", "--ro-bind /usr /usr", "--bind " + root + " " + root, "--chdir " + root, "--setenv HOME " + root, "--die-with-parent", "-- bash -c echo hi"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("bwrap argv = %q, want %q", argv, want)
		}
	}
}

func TestBashSandboxedMissingBwrap(t *testing.T) {
	e := &Executor{Root: t.TempDir(), Sandbox: true, BwrapPath: "/nonexistent/bwrap"}
	if _, err := e.Bash(context.Background(), BashArgs{Command: "echo hi"}); err == nil {
		t.Fatal("sandboxed bash with no bwrap should fail")
	}
}

func TestProbeSandbox(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	if err := ProbeSandbox("bwrap"); err != nil {
		t.Fatalf("probe real bwrap: %v", err)
	}
	if err := ProbeSandbox("/nonexistent/bwrap"); err == nil {
		t.Fatal("probe with no bwrap should fail")
	}
}

// The real sandbox: /etc is read-only, the parent of the root is not
// visible, HOME and pwd are the root, /tmp is fresh per call, and a
// file written inside the root is visible on the host.
func TestBashSandboxReal(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	root := t.TempDir()
	e := &Executor{Root: root, Sandbox: true}
	ctx := context.Background()
	cases := []struct {
		cmd  string
		want string
		fail bool
	}{
		{"pwd", root + "\n", false},
		{"echo $HOME", root + "\n", false},
		{"touch /etc/sandbox-test", "", true},
		{"test -d /home", "", true},
		// ../escape.txt "succeeds" inside the sandbox, but only into a
		// scratch parent directory that vanishes with the sandbox —
		// the host check below proves nothing escaped.
		{"touch ../escape.txt", "", false},
		{"echo sandboxed > hello.txt && cat hello.txt", "sandboxed\n", false},
	}
	for _, c := range cases {
		out, err := e.Bash(ctx, BashArgs{Command: c.cmd})
		if c.fail {
			if err == nil {
				t.Fatalf("sandboxed %q should fail, got %q", c.cmd, out)
			}
			continue
		}
		if err != nil || out != c.want {
			t.Fatalf("sandboxed %q = %q, err %v, want %q", c.cmd, out, err, c.want)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil || string(data) != "sandboxed\n" {
		t.Fatalf("hello.txt on the host = %q, err %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("escape.txt must not exist outside the root")
	}
	// /tmp is fresh per call: a marker from one call is gone in the
	// next. (The root's parent dir shows up in /tmp as an empty
	// mountpoint placeholder — that is bwrap creating the bind's
	// parents, not a leak.)
	if _, err := e.Bash(ctx, BashArgs{Command: "touch /tmp/marker"}); err != nil {
		t.Fatal(err)
	}
	out, err := e.Bash(ctx, BashArgs{Command: "ls /tmp"})
	if err != nil || strings.Contains(out, "marker") {
		t.Fatalf("second call /tmp = %q, err %v (marker must be gone)", out, err)
	}
}
