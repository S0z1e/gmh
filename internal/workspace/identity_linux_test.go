package workspace

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Install the real seccomp restriction only in an isolated helper process. It
// must not alter the test runner's or container runtime's syscall permissions.
func TestSeccompDeniedBirthtimeRemainsUntrusted(t *testing.T) {
	if os.Getenv("GMH_TRUST_SECCOMP_HELPER") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSeccompDeniedBirthtimeRemainsUntrusted$", "-test.v")
		cmd.Env = append(os.Environ(), "GMH_TRUST_SECCOMP_HELPER=1", "GMH_TRUST_TEST_ROOT="+t.TempDir(), "HOME="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seccomp helper: %v\n%s", err, out)
		}
		return
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_STATX, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.Syscall6(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog)), 0, 0, 0)
	runtime.KeepAlive(filter)
	if errno != 0 {
		t.Fatal(errno)
	}
	id, err := Identify(os.Getenv("GMH_TRUST_TEST_ROOT"))
	if err != nil {
		t.Fatalf("untrusted startup failed: %v", err)
	}
	if id.FileID != "" {
		t.Fatal("trusted metadata unexpectedly available under seccomp")
	}
	if trusted, err := Trusted(id); err != nil || trusted {
		t.Fatalf("unavailable incarnation trusted: %v %v", trusted, err)
	}
	if err := Decide(id, "trust"); err == nil {
		t.Fatal("saved trust without incarnation")
	}
}
