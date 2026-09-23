//go:build !(linux || darwin || freebsd)

package download

// diskFree is unknown on this platform; the free-space check is skipped.
func diskFree(string) (int64, bool) { return 0, false }
