package mux_test

import (
	"bytes"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/mux"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

func TestMux(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Mux Suite")
}

var _ = Describe("Output pattern expansion [REQ-RUN-003]", func() {
	f := mux.PatternFields{
		JobID: 4711, ArrayJobID: 4720, ArrayTaskID: 7, StepID: 2, Rank: 3,
		NodeID: 1, NodeName: "node002", JobName: "myjob", User: "alice",
	}

	DescribeTable("substitutes SLURM verbs",
		func(pattern, want string) {
			Expect(mux.ExpandPattern(pattern, f)).To(Equal(want))
		},
		Entry("job id", "out.%j.log", "out.4711.log"),
		Entry("job.step", "out.%J.log", "out.4711.2.log"),
		Entry("rank and node", "out.%J.%t.%n.log", "out.4711.2.3.1.log"),
		Entry("node name", "%N.out", "node002.out"),
		Entry("step id", "s%s.log", "s2.log"),
		Entry("literal percent", "100%%done", "100%done"),
		Entry("unknown verb kept", "%z.log", "%z.log"),
		Entry("array job/task 0-based (submitit)", "%A_%a_%t_log.out", "4720_7_3_log.out"),
		Entry("job name and user", "%x-%u.log", "myjob-alice.log"),
		// SLURM allows a zero-pad width; we cannot pad, but the verb must still
		// expand or every rank writes to one literal filename.
		Entry("zero-pad width dropped, verb expands", "run_%3a.log", "run_7.log"),
		Entry("zero-pad on job id", "j%2j.out", "j4711.out"),
		Entry("escaped percent before a digit", "100%%3a.log", "100%3a.log"),
		Entry("trailing width with no verb", "out.%3", "out.%"),
	)
})

var _ = Describe("Output demux [REQ-RUN-020]", func() {
	out := func(rank uint32, flags uint8, s string) proto.Frame {
		return proto.Frame{Type: proto.FrameOut, Rank: rank, Flags: flags, Payload: []byte(s)}
	}

	It("writes whole lines to stdout", func() {
		var buf bytes.Buffer
		d := mux.NewDemux(&buf, &buf, false)
		Expect(d.Handle(out(0, proto.FlagEOL, "hello"))).To(Succeed())
		Expect(buf.String()).To(Equal("hello\n"))
	})

	It("prefixes each line with the rank under -l", func() {
		var buf bytes.Buffer
		d := mux.NewDemux(&buf, &buf, true)
		Expect(d.Handle(out(3, proto.FlagEOL, "hi"))).To(Succeed())
		Expect(buf.String()).To(Equal("3: hi\n"))
	})

	It("reassembles a long line split across chunks, labeling once", func() {
		var buf bytes.Buffer
		d := mux.NewDemux(&buf, &buf, true)
		Expect(d.Handle(out(1, 0, "abc"))).To(Succeed()) // no EOL
		Expect(d.Handle(out(1, proto.FlagEOL, "def"))).To(Succeed())
		Expect(buf.String()).To(Equal("1: abcdef\n"))
	})

	It("routes stderr separately", func() {
		var so, se bytes.Buffer
		d := mux.NewDemux(&so, &se, false)
		Expect(d.Handle(out(0, proto.FlagStderr|proto.FlagEOL, "err"))).To(Succeed())
		Expect(so.String()).To(Equal(""))
		Expect(se.String()).To(Equal("err\n"))
	})

	It("flushes a dangling partial line with a trailing newline", func() {
		var buf bytes.Buffer
		d := mux.NewDemux(&buf, &buf, false)
		Expect(d.Handle(out(0, 0, "partial"))).To(Succeed()) // no EOL, no more
		Expect(buf.String()).To(Equal("partial"))
		Expect(d.Flush()).To(Succeed())
		Expect(buf.String()).To(Equal("partial\n"))
	})
})

// writeRecorder records the byte slice of every Write call separately, so a spec
// can assert how many calls a line cost rather than only what was written.
type writeRecorder struct{ writes [][]byte }

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	return len(p), nil
}

var _ = Describe("Output demux write atomicity [REQ-RUN-020]", func() {
	// A job script may background several srun steps that all inherited the same
	// stdout; Ray's head-plus-workers recipe does exactly that. The Demux mutex
	// orders writes within ONE srun process and can do nothing about another one,
	// so a line split across several Write calls lets a concurrent srun insert its
	// own line between the payload and the newline. Observed on a four-node GCP
	// cluster: two workers' lines arrived concatenated, and a line count saw three
	// steps as two. One Write per frame is what keeps concurrent steps apart.
	frame := func(rank uint32, payload string, eol bool) proto.Frame {
		f := proto.Frame{Type: proto.FrameOut, Rank: rank, Payload: []byte(payload)}
		if eol {
			f.Flags |= proto.FlagEOL
		}
		return f
	}

	It("writes a complete line in exactly one call", func() {
		rec := &writeRecorder{}
		d := mux.NewDemux(rec, rec, false)
		Expect(d.Handle(frame(0, "RAYWORKER on node001", true))).To(Succeed())
		Expect(rec.writes).To(HaveLen(1))
		Expect(string(rec.writes[0])).To(Equal("RAYWORKER on node001\n"))
	})

	It("writes a labelled line in exactly one call", func() {
		rec := &writeRecorder{}
		d := mux.NewDemux(rec, rec, true)
		Expect(d.Handle(frame(3, "hello", true))).To(Succeed())
		Expect(rec.writes).To(HaveLen(1))
		Expect(string(rec.writes[0])).To(Equal("3: hello\n"))
	})

	It("writes a continuation chunk in exactly one call", func() {
		rec := &writeRecorder{}
		d := mux.NewDemux(rec, rec, true)
		Expect(d.Handle(frame(1, "part", false))).To(Succeed())
		Expect(d.Handle(frame(1, "rest", true))).To(Succeed())
		Expect(rec.writes).To(HaveLen(2))
		Expect(string(rec.writes[0])).To(Equal("1: part"))
		Expect(string(rec.writes[1])).To(Equal("rest\n"), "a continuation is not re-labelled")
	})
})
