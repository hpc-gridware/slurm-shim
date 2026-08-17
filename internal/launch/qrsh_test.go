package launch

import (
	"context"
	"io"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

func TestLaunch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Launch Suite")
}

var _ = Describe("qrsh argv and env [REQ-RUN-009]", func() {
	It("builds the tight-integration argv with the token via -v", func() {
		args := buildQrshArgs("/opt/shim", "node002", "ENVB64", "deadbeef")
		Expect(args).To(Equal([]string{
			"-inherit", "-nostdin", "-noshell",
			"-v", "SLURM_SHIM_TOKEN=deadbeef",
			"node002",
			"/opt/shim", "stepper", "--envelope", "ENVB64",
		}))
	})

	It("scrubs QRSH_WRAPPER and SGE_RSH_COMMAND from the child env [SI-23]", func() {
		in := []string{"PATH=/bin", "QRSH_WRAPPER=/x", "HOME=/h", "SGE_RSH_COMMAND=/y", "FOO=bar"}
		Expect(qrshEnv(in)).To(Equal([]string{"PATH=/bin", "HOME=/h", "FOO=bar"}))
	})

	It("redacts the token argv element for logging [REQ-CHN-003]", func() {
		args := buildQrshArgs("/opt/shim", "node002", "E", "secrettoken")
		red := redactArgs(args)
		Expect(red).To(ContainElement("SLURM_SHIM_TOKEN=<redacted>"))
		for _, a := range red {
			Expect(a).NotTo(ContainSubstring("secrettoken"))
		}
	})
})

var _ = Describe("qrsh rejection classifier [REQ-RUN-024]", func() {
	DescribeTable("classifies captured qrsh stderr",
		func(stderr string, want rejectionKind) {
			Expect(classifyRejection(stderr)).To(Equal(want))
		},
		Entry("no JOB_ID (misconfig) is fatal",
			`error: "qrsh" called with option "-inherit", but "JOB_ID" not set in environment`, rejectFatal),
		Entry("missing SGE_TASK_ID is fatal",
			`error: executing task of job 999999 failed: missing "SGE_TASK_ID" in environment`, rejectFatal),
		Entry("execd did not accept task is a retryable race",
			`error: executing task of job 999999 failed: execution daemon on host "ocs-worker1" didn't accept task`, rejectJobRace),
		Entry("job does not exist is a retryable race",
			`error: job 4711 does not exist`, rejectJobRace),
		Entry("no suitable queues is slot exhaustion",
			`error: no suitable queues`, rejectSlots),
		Entry("unknown text defaults to a bounded race retry, not fatal",
			`error: something unexpected`, rejectJobRace),
	)
})

// fakeProc drives QrshLauncher.Start deterministically. Each settle() call pops
// the next scripted outcome.
type fakeProc struct {
	exits  bool
	stderr string
	killed bool
}

func (p *fakeProc) settle(time.Duration) (bool, string) { return p.exits, p.stderr }
func (p *fakeProc) Wait() error                         { return nil }
func (p *fakeProc) Kill() error                         { p.killed = true; return nil }

// scriptedLauncher builds a QrshLauncher whose spawn returns the given procs in
// order, with a virtual clock so retry deadlines are deterministic.
func scriptedLauncher(procs []*fakeProc, tick time.Duration) (*QrshLauncher, *int) {
	calls := 0
	i := 0
	clock := time.Unix(0, 0)
	l := &QrshLauncher{
		Self:   "/shim",
		Stderr: io.Discard,
		spawn: func(_ context.Context, _, _ []string, _ io.Writer) (childProc, error) {
			calls++
			p := procs[i]
			if i < len(procs)-1 {
				i++
			}
			return p, nil
		},
		sleep: func(time.Duration) { clock = clock.Add(tick) },
		now:   func() time.Time { return clock },
	}
	return l, &calls
}

var _ = Describe("QrshLauncher.Start retry policy [REQ-RUN-024]", func() {
	env := proto.Envelope{JobID: 4711, StepID: 1, Host: "node002", NodeID: 1, Dial: "127.0.0.1:5000"}

	It("returns a handle when qrsh survives the settle window", func() {
		l, calls := scriptedLauncher([]*fakeProc{{exits: false}}, time.Second)
		h, err := l.Start(context.Background(), "node002", env, "tok")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Host()).To(Equal("node002"))
		Expect(*calls).To(Equal(1))
	})

	It("retries a job-race rejection then succeeds [SI-09]", func() {
		procs := []*fakeProc{
			{exits: true, stderr: `error: job 4711 does not exist`},
			{exits: false},
		}
		l, calls := scriptedLauncher(procs, time.Second)
		h, err := l.Start(context.Background(), "node002", env, "tok")
		Expect(err).NotTo(HaveOccurred())
		Expect(h).NotTo(BeNil())
		Expect(*calls).To(Equal(2))
	})

	It("gives up on a job-race that never clears within the grace", func() {
		// 20s tick per retry exceeds the 10s race grace on the first sleep.
		procs := []*fakeProc{{exits: true, stderr: `error: job 4711 does not exist`}}
		l, _ := scriptedLauncher(procs, 20*time.Second)
		_, err := l.Start(context.Background(), "node002", env, "tok")
		Expect(err).To(MatchError(ContainSubstring("job not yet known")))
	})

	It("retries slot exhaustion under its own bound then gives up [SI-55]", func() {
		procs := []*fakeProc{{exits: true, stderr: `error: no suitable queues`}}
		// 6-minute tick exceeds the 5-minute default slot bound after one sleep.
		l, _ := scriptedLauncher(procs, 6*time.Minute)
		_, err := l.Start(context.Background(), "node002", env, "tok")
		Expect(err).To(MatchError(ContainSubstring("slots unavailable")))
	})

	It("fails immediately on a fatal rejection", func() {
		procs := []*fakeProc{{exits: true, stderr: `error: "qrsh" called with option "-inherit", but "JOB_ID" not set in environment`}}
		l, calls := scriptedLauncher(procs, time.Second)
		_, err := l.Start(context.Background(), "node002", env, "tok")
		Expect(err).To(MatchError(ContainSubstring("failed")))
		Expect(*calls).To(Equal(1))
	})
})
