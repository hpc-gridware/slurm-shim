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

package proto_test

import (
	"context"
	"net"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// tcpPair returns srun's and the stepper's end of one authenticated loopback
// control channel.
func tcpPair() (*proto.Conn, *proto.Conn) {
	token, err := proto.NewToken()
	Expect(err).NotTo(HaveOccurred())
	srv, err := proto.Listen("127.0.0.1:0", token)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(srv.Close)
	stepperSide, err := proto.Dial(srv.Addr(), token, "node002")
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srunSide, err := srv.Accept(ctx)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = srunSide.Close(); _ = stepperSide.Close() })
	return srunSide, stepperSide
}

var _ = Describe("control channel liveness [REQ-CHN-004]", func() {
	It("enables kernel liveness on a TCP control channel, both ends", func() {
		srunSide, stepperSide := tcpPair()
		Expect(srunSide.SetLiveness(60*time.Second, 5*time.Second)).To(Succeed())
		Expect(stepperSide.SetLiveness(45*time.Second, 5*time.Second)).To(Succeed())
	})

	It("accepts sub-second settings (keepalive is rounded up to a second)", func() {
		srunSide, _ := tcpPair()
		Expect(srunSide.SetLiveness(600*time.Millisecond, 200*time.Millisecond)).To(Succeed())
	})

	It("refuses non-positive settings rather than silently disabling detection", func() {
		srunSide, _ := tcpPair()
		Expect(srunSide.SetLiveness(0, 5*time.Second)).NotTo(Succeed())
		Expect(srunSide.SetLiveness(60*time.Second, 0)).NotTo(Succeed())
	})

	It("keeps a healthy idle channel open well past the timeout", func() {
		// The peer's kernel answers keepalive probes even while nothing is sent.
		srunSide, stepperSide := tcpPair()
		Expect(srunSide.SetLiveness(2*time.Second, time.Second)).To(Succeed())
		got := make(chan error, 1)
		go func() { _, err := srunSide.Recv(); got <- err }()
		Consistently(got, "4s").ShouldNot(Receive())
		Expect(stepperSide.Send(proto.Frame{Type: proto.FrameReady})).To(Succeed())
		Eventually(got, "1s").Should(Receive(BeNil()))
	})

	It("classifies a timed-out channel, and only that, as a liveness timeout", func() {
		Expect(proto.IsLivenessTimeout(&net.OpError{Op: "read", Err: syscall.ETIMEDOUT})).To(BeTrue())
		Expect(proto.IsLivenessTimeout(net.ErrClosed)).To(BeFalse())
	})
})
