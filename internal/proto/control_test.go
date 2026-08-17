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
