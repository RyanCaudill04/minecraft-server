//go:build !linux && !darwin

package backups

func freeBytes(string) (uint64, bool) { return 0, false }
