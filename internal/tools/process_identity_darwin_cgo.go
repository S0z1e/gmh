//go:build darwin && cgo

package tools

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <stdlib.h>

static int check_pid_vnode_file(int pid, uint64_t want_dev, uint64_t want_ino) {
	int buf_size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (buf_size <= 0) {
		return 0;
	}
	struct proc_fdinfo *fds = (struct proc_fdinfo *)malloc((size_t)buf_size);
	if (fds == NULL) {
		return 0;
	}
	int actual = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, buf_size);
	int matched = 0;
	if (actual > 0) {
		int count = actual / (int)sizeof(struct proc_fdinfo);
		for (int i = 0; i < count; i++) {
			if (fds[i].proc_fdtype != PROX_FDTYPE_VNODE) {
				continue;
			}
			struct vnode_fdinfowithpath vfi;
			int n = proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDVNODEPATHINFO, &vfi, (int)sizeof(vfi));
			if (n == (int)sizeof(vfi)) {
				if ((uint64_t)vfi.pvip.vip_vi.vi_stat.vst_dev == want_dev &&
				    (uint64_t)vfi.pvip.vip_vi.vi_stat.vst_ino == want_ino) {
					matched = 1;
					break;
				}
			}
		}
	}
	free(fds);
	return matched;
}
*/
import "C"

func darwinProcessHasFile(pid int, wantDev, wantIno uint64) bool {
	if pid <= 0 || wantIno == 0 {
		return false
	}
	return C.check_pid_vnode_file(C.int(pid), C.uint64_t(wantDev), C.uint64_t(wantIno)) == 1
}
