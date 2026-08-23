//go:build windows

package bootp

import "syscall"

// enableBroadcast turns on SO_BROADCAST for the raw socket fd. On Windows a
// socket is a Handle, not an int, so SetsockoptInt takes syscall.Handle.
func enableBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}
