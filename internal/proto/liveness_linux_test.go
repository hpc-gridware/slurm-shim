package proto

import (
	"net"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("control channel liveness on Linux [REQ-CHN-004]", func() {
	It("bounds unacknowledged data with TCP_USER_TIMEOUT", func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(ln.Close)
		go func() { c, _ := net.Dial("tcp", ln.Addr().String()); DeferCleanup(c.Close) }()
		nc, err := ln.Accept()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(nc.Close)
		c := &Conn{nc: nc}

		Expect(c.SetLiveness(60*time.Second, 5*time.Second)).To(Succeed())

		raw, err := nc.(*net.TCPConn).SyscallConn()
		Expect(err).NotTo(HaveOccurred())
		var got int
		Expect(raw.Control(func(fd uintptr) {
			got, err = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
		})).To(Succeed())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(60000))
	})
})
