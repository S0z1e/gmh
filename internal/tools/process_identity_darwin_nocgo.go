//go:build darwin && !cgo

package tools

func darwinProcessHasFile(pid int, wantDev, wantIno uint64) bool {
	return false
}
