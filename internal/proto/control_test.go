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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

var _ = Describe("Control channel [REQ-CHN-002]", func() {
	var (
		srv   *proto.Server
		token string
	)

	BeforeEach(func() {
		var err error
		token, err = proto.NewToken()
		Expect(err).NotTo(HaveOccurred())
		srv, err = proto.Listen("127.0.0.1:0", token)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(srv.Close)
	})

	It("authenticates a stepper and exchanges frames both ways [REQ-CHN-002]", func() {
		client, err := proto.Dial(srv.Addr(), token, "node002")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = client.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := srv.Accept(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.Host).To(Equal("node002"))

		// srun -> stepper: push a spec.
		Expect(conn.Send(proto.Frame{Type: proto.FrameSpec, Payload: []byte("spec")})).To(Succeed())
		got, err := client.Recv()
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Type).To(Equal(proto.FrameSpec))

		// stepper -> srun: report a rank exit.
		Expect(client.Send(proto.Frame{Type: proto.FrameRankExit, Rank: 3, Payload: proto.EncodeInt32(7)})).To(Succeed())
		got, err = conn.Recv()
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Type).To(Equal(proto.FrameRankExit))
		Expect(got.Rank).To(Equal(uint32(3)))
		Expect(proto.DecodeInt32(got.Payload)).To(Equal(int32(7)))
	})

	It("drops an authenticated stepper nobody accepted as soon as the server closes", func() {
		// srun aborting a launch closes the server; a stepper waiting for its
		// StepSpec must see EOF at once, not after HelloTimeout (10s).
		client, err := proto.Dial(srv.Addr(), token, "node002")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = client.Close() }()
		// Let the server authenticate it; nothing ever calls Accept.
		time.Sleep(100 * time.Millisecond)

		Expect(srv.Close()).To(Succeed())

		dropped := make(chan error, 1)
		go func() { _, err := client.Recv(); dropped <- err }()
		Eventually(dropped, "1s").Should(Receive(HaveOccurred()))
	})

	It("can be closed twice", func() {
		Expect(srv.Close()).To(Succeed())
		Expect(srv.Close()).To(Succeed())
	})

	It("rejects a connection presenting the wrong token [REQ-CHN-002]", func() {
		client, err := proto.Dial(srv.Addr(), "wrong-token", "attacker")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = client.Close() }()

		// The server drops the connection; Accept must not deliver it.
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err = srv.Accept(ctx)
		Expect(err).To(Equal(context.DeadlineExceeded))
	})
})
