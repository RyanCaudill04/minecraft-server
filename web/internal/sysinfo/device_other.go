//go:build !linux

package sysinfo

import "errors"

// Device reports the device number of the filesystem holding path. Only
// Linux has /proc/diskstats to look it up in, so elsewhere it fails.
func Device(string) (major, minor uint32, err error) {
	return 0, 0, errors.New("device numbers are only read on Linux")
}
