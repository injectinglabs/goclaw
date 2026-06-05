//go:build !windows

package diskutil

import "syscall"

// Fraction returns used/total for the filesystem hosting path
// (1.0 = volume is full). Linux/Darwin via statfs(2).
func Fraction(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return 0, nil
	}
	used := st.Blocks - st.Bavail
	return float64(used) / float64(st.Blocks), nil
}
