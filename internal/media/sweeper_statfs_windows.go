//go:build windows

package media

// diskUsageFraction stub for Windows. Disk-pressure LRU isn't supported
// on Windows builds — goclaw runs in Linux containers in prod; Windows
// builds exist only for local dev where the FSBackend (not S3Backend)
// is typically used and disk pressure isn't a concern.
// Returning 0 means "below any limit", so the sweeper's TTL phase
// still runs while the disk-pressure phase is a no-op.
func diskUsageFraction(_ string) (float64, error) {
	return 0, nil
}
