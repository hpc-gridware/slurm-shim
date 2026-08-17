package proto_test

import (
	"bytes"
	"io"
	"math/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

var _ = Describe("Frame codec", func() {
	It("round-trips a frame byte-identically [REQ-TST-004]", func() {
		buf := &bytes.Buffer{}
		w := proto.NewFrameWriter(buf)
		f := proto.Frame{Type: proto.FrameOut, Rank: 7, Flags: proto.FlagStderr | proto.FlagEOL, Payload: []byte("hello\tworld")}
		Expect(w.Write(f)).To(Succeed())

		got, err := proto.NewFrameReader(buf).Read()
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(f))
	})

	It("preserves arbitrary binary payloads including long lines [REQ-RUN-020]", func() {
		rng := rand.New(rand.NewSource(GinkgoRandomSeed()))
		buf := &bytes.Buffer{}
		w := proto.NewFrameWriter(buf)

		var written []proto.Frame
		for i := 0; i < 2000; i++ {
			n := rng.Intn(70000) // spans the 64 KiB chunk boundary
			payload := make([]byte, n)
			rng.Read(payload)
			f := proto.Frame{
				Type:    proto.FrameOut,
				Rank:    uint32(rng.Intn(256)),
				Flags:   uint8(rng.Intn(4)),
				Payload: payload,
			}
			Expect(w.Write(f)).To(Succeed())
			written = append(written, f)
		}

		r := proto.NewFrameReader(buf)
		for _, want := range written {
			got, err := r.Read()
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Type).To(Equal(want.Type))
			Expect(got.Rank).To(Equal(want.Rank))
			Expect(got.Flags).To(Equal(want.Flags))
			Expect(bytes.Equal(got.Payload, want.Payload)).To(BeTrue())
		}
		_, err := r.Read()
		Expect(err).To(Equal(io.EOF))
	})

	It("carries int32 exit codes and signals in payloads", func() {
		Expect(proto.DecodeInt32(proto.EncodeInt32(143))).To(Equal(int32(143)))
		Expect(proto.DecodeInt32(proto.EncodeInt32(-1))).To(Equal(int32(-1)))
		Expect(proto.DecodeInt32([]byte{1})).To(Equal(int32(0)))
	})

	It("rejects a payload length beyond the maximum", func() {
		// A header claiming a 2 MiB payload must be refused, not allocated.
		bad := []byte{byte(proto.FrameOut), 0, 0, 0, 0, 0, 0, 0x20, 0, 0}
		_, err := proto.NewFrameReader(bytes.NewReader(bad)).Read()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Envelope and StepSpec codecs [REQ-RUN-009]", func() {
	It("round-trips the argv envelope", func() {
		e := proto.Envelope{JobID: 4711, StepID: 2, Host: "node002", NodeID: 1, Dial: "10.0.0.11:34567"}
		tok, err := proto.EncodeEnvelope(e)
		Expect(err).NotTo(HaveOccurred())
		got, err := proto.DecodeEnvelope(tok)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(e))
	})

	It("round-trips the StepSpec", func() {
		s := proto.StepSpec{
			Env:     []string{"HOME=/home/alice", "SLURM_JOB_ID=4711"},
			Command: []string{"hostname"},
			Label:   true,
			Ranks: []proto.RankSpec{
				{Rank: 0, Local: 0, NodeID: 0, Cpuset: "0-3", EnvDelta: []string{"SLURM_PROCID=0"}},
			},
		}
		b, err := proto.EncodeSpec(s)
		Expect(err).NotTo(HaveOccurred())
		got, err := proto.DecodeSpec(b)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(s))
	})
})
