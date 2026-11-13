package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// sandboxArgs is the bwrap invocation that confines one bash call to
// the workspace root. bwrap starts from an EMPTY root, so every path
// the sandbox needs is bound in explicitly: /usr and /etc (read-only),
// /bin, /lib, /lib64 and /sbin (read-only binds of the host's — on a
// merged-/usr system these resolve into /usr, which is also how the
// dynamic loader finds /lib64/ld-linux), /proc, /dev, a fresh /tmp,
// and the workspace itself (read-write). HOME is the workspace, so
// dotfiles land inside the jail. The order matters: --tmpfs / must
// come first, or the host's /home and friends would still be visible
// underneath.
//
// What it does NOT do: there is NO pid namespace — mounting /proc in
// a new pid namespace owned by a new user namespace is refused by
// the kernel in containers (this VM included), so the sandbox shares
// the host's pid namespace and `ps` lists host processes. That is a
// known, minor leak: the user-namespace uid mapping still stops the
// sandboxed process from signalling host processes, and the
// filesystem boundary is unaffected. The network is not unshared, so
// curl and friends keep working (and the proxy env vars pass
// through); each call gets a FRESH /tmp, so nothing persists there
// between calls; and only bash is sandboxed — read/write/edit run
// in-process and are jailed to the root by the harness itself.
// Tools installed outside /usr (a Go toolchain in your home
// directory, say) are not visible inside the sandbox.
func sandboxArgs(root, dir string) []string {
	return []string{
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
		"--chdir", dir,
		"--setenv", "HOME", root,
		"--unshare-user",
		"--unshare-ipc",
		"--die-with-parent",
	}
}

// ProbeSandbox checks that bwrap can actually create a sandbox here,
// not just that the binary exists: unprivileged user namespaces may
// be disabled, in which case every sandboxed call would fail. It
// runs a trivial `true` inside the same namespace shape the real
// calls use, with a 10s timeout.
func ProbeSandbox(bwrap string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrap,
		"--tmpfs", "/",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/sbin", "/sbin",
		"--proc", "/proc",
		"--unshare-user", "--unshare-ipc",
		"--", "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bwrap cannot create a sandbox here (unprivileged user namespaces may be disabled): %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
