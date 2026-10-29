//go:build !unix

package nfs_test

import "net"

// sendBuffer reports that the buffer size cannot be read here.
func sendBuffer(net.PacketConn) int { return -1 }
