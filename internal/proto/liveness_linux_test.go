/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

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
		// Dial in a goroutine, close from the spec: a DeferCleanup registered by
		// the goroutine can race the end of the spec, which Ginkgo refuses.
		dialed := make(chan net.Conn, 1)
		go func() {
			c, _ := net.Dial("tcp", ln.Addr().String()) // nil on error
			dialed <- c
		}()
		nc, err := ln.Accept()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(nc.Close)
		if client := <-dialed; client != nil {
			DeferCleanup(client.Close)
		}
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
