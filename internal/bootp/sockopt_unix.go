//go:build unix

package bootp

import "syscall"

// enableBroadcast turns on SO_BROADCAST for the raw socket fd. On Unix the
// descriptor is an int.
func enableBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}
