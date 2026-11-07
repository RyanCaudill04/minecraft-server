//go:build linux

package sysinfo

import (
	"os"
	"syscall"
)

// Device reports the device number of the filesystem holding path.
func Device(path string) (major, minor uint32, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	// Dev is uint32 on a few architectures; this is glibc's major() and
	// minor().
	dev := uint64(st.Dev)
	major = uint32((dev>>8)&0xfff | (dev>>32)&^0xfff)
	minor = uint32(dev&0xff | (dev>>12)&^0xff)
	return major, minor, nil
}
