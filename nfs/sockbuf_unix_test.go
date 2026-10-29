//go:build unix

package nfs_test

import (
	"net"
	"syscall"
)

// sendBuffer reads SO_SNDBUF of pc, or -1 when it cannot.
func sendBuffer(pc net.PacketConn) int {
	rc, err := pc.(*net.UDPConn).SyscallConn()
	if err != nil {
		return -1
	}
	size := -1
	rc.Control(func(fd uintptr) {
		if v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF); err == nil {
			size = v
		}
	})
	return size
}
