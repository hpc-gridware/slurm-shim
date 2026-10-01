package proto

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT (linux/tcp.h); syscall does not export it.
const tcpUserTimeout = 0x12

// setUserTimeout bounds how long sent data may stay unacknowledged, so a peer
// that vanishes while this side has data in flight (a busy output stream) is
// detected within timeout instead of after the kernel's retransmission limit
// (tcp_retries2, about 15 minutes). Since Linux 5.11 a peer that keeps ACKing
// zero-window probes (alive, just not reading) is not aborted.
func setUserTimeout(tc *net.TCPConn, timeout time.Duration) error {
	raw, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(timeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
