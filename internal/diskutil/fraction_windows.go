//go:build windows

package diskutil

// Fraction stub for Windows builds. Disk-pressure eviction isn't a
// concern there because goclaw runs in Linux containers everywhere we
// deploy; the Windows build is for local dev only. Returning 0 (well
// below any limit) keeps callers' "if usage < limit → no-op" branches
// taking the no-op path.
func Fraction(_ string) (float64, error) {
	return 0, nil
}
