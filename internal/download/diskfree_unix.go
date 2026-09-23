//go:build linux || darwin || freebsd

package download

import "syscall"

// diskFree reports bytes available to unprivileged users at path.
func diskFree(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
