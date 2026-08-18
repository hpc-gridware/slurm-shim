package launch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// Launch-retry bounds (REQ-RUN-024). The job-race grace covers the async
// slave-execd notification window (SI-09); the slot-retry default bounds the
// SI-55 over-slot backoff and is excluded from launch_timeout.
const (
	jobRaceGrace     = 10 * time.Second
	jobRaceBackoff   = 500 * time.Millisecond
	slotRetryBackoff = 2 * time.Second
	defaultSlotRetry = 5 * time.Minute
	// rejectionWindow is how long Start watches a freshly spawned qrsh for an
	// early exit. GE execd rejects an unlaunchable task within milliseconds; a
	// qrsh still alive after this window is running the stepper, which then dials
	// the control channel (READY is bounded separately by launch_timeout).
	rejectionWindow = 2 * time.Second
	// slotRetryMessage is the SLURM-style line printed while retrying a
	// slot-exhaustion rejection (SI-44).
	slotRetryMessage = "srun: Job step creation temporarily disabled, retrying"
)

// QrshLauncher runs each remote stepper under GE tight integration via
// `qrsh -inherit` (D-5, REQ-RUN-009), so execd owns accounting and cleanup.
type QrshLauncher struct {
	Self      string    // absolute shim path (target argv[0] on the remote host)
	Stderr    io.Writer // qrsh child stderr is teed here
	SlotRetry time.Duration

	// Seams for deterministic tests; nil selects the production behavior.
	spawn func(ctx context.Context, args, env []string, tee io.Writer) (childProc, error)
	sleep func(time.Duration)
	now   func() time.Time
}

// childProc is a spawned qrsh process. settle reports an early exit within d.
type childProc interface {
	settle(d time.Duration) (exited bool, stderrTail string)
	Wait() error
	Kill() error
}

// Start launches one stepper on host and, on an early GE rejection, retries per
// REQ-RUN-024 before surfacing a launcher failure.
func (l QrshLauncher) Start(ctx context.Context, host string, env proto.Envelope, token string) (Handle, error) {
	arg, err := proto.EncodeEnvelope(env)
	if err != nil {
		return nil, err
	}
	args := buildQrshArgs(l.Self, host, arg, token)
	childEnv := qrshEnv(os.Environ())

	slotBound := l.SlotRetry
	if slotBound <= 0 {
		slotBound = defaultSlotRetry
	}
	now := l.now
	if now == nil {
		now = time.Now
	}
	sleep := l.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	spawn := l.spawn
	if spawn == nil {
		spawn = spawnQrsh
	}

	raceDeadline := now().Add(jobRaceGrace)
	slotDeadline := now().Add(slotBound)

	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("qrsh launch on %s cancelled: %w", host, err)
		}
		proc, err := spawn(ctx, args, childEnv, l.Stderr)
		if err != nil {
			return nil, fmt.Errorf("spawning qrsh for %s: %w", host, err)
		}
		exited, tail := proc.settle(rejectionWindow)
		if !exited {
			return &qrshHandle{host: host, proc: proc}, nil
		}

		switch classifyRejection(tail) {
		case rejectJobRace:
			if now().After(raceDeadline) {
				return nil, fmt.Errorf("qrsh launch on %s kept failing (job not yet known to execd): %s", host, oneLine(tail))
			}
			sleep(jobRaceBackoff)
		case rejectSlots:
			if now().After(slotDeadline) {
				return nil, fmt.Errorf("qrsh launch on %s: slots unavailable past slot_retry bound: %s", host, oneLine(tail))
			}
			fmt.Fprintln(l.Stderr, slotRetryMessage)
			sleep(slotRetryBackoff)
		default:
			// Redact the token from the diagnostic command (REQ-CHN-003, SI-51).
			return nil, fmt.Errorf("qrsh launch on %s failed: %s (command: qrsh %s)",
				host, oneLine(tail), strings.Join(redactArgs(args), " "))
		}
	}
}

// buildQrshArgs builds the qrsh argv (REQ-RUN-009). The command line carries only
// routing data in the envelope - never the environment - and the per-step token
// travels via -v (REQ-CHN-002), landing in the remote owner-readable environ.
// Launched without -V so no client environment leaks.
func buildQrshArgs(self, host, envelopeArg, token string) []string {
	return []string{
		"-inherit", "-nostdin", "-noshell",
		"-v", "SLURM_SHIM_TOKEN=" + token,
		host,
		self, "stepper", "--envelope", envelopeArg,
	}
}

// qrshEnv is the child environment for qrsh: the caller's environment with
// QRSH_WRAPPER unset and SGE_RSH_COMMAND removed so neither redirects the
// builtin transport (SI-23).
func qrshEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if key == "QRSH_WRAPPER" || key == "SGE_RSH_COMMAND" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// redactArgs returns a copy of args safe to log: the token value in
// `-v SLURM_SHIM_TOKEN=<hex>` is masked (REQ-CHN-003, SI-51).
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "SLURM_SHIM_TOKEN=") {
			out[i] = "SLURM_SHIM_TOKEN=<redacted>"
		} else {
			out[i] = a
		}
	}
	return out
}

// rejectionKind classifies an early qrsh exit (REQ-RUN-024).
type rejectionKind int

const (
	rejectFatal   rejectionKind = iota // not retryable (misconfig, unknown)
	rejectJobRace                      // slave execd has not yet learned the job (SI-09)
	rejectSlots                        // slots temporarily exhausted (SI-44/SI-55)
)

// classifyRejection maps qrsh stderr to a retry class using signatures captured
// on a live OCS cluster (testdata/qrsh_rejections.txt).
//
// Only the misconfiguration (fatal) and async-race signatures are confirmed
// against real qrsh output. The real over-slot rejection (SI-55) has NOT been
// captured live yet, so the slot branch keys on the single best-guess phrase and
// the default is the bounded 10s race retry (never immediate fatal): an
// unrecognized transient rejection then gets a short retry instead of aborting
// the step, and a genuinely permanent error still fails within the race grace.
func classifyRejection(stderr string) rejectionKind {
	s := strings.ToLower(stderr)
	switch {
	// Misconfiguration - never retry (real signatures: JOB_ID/SGE_TASK_ID unset).
	case strings.Contains(s, "not set in environment"),
		strings.Contains(s, `missing "sge_task_id"`),
		strings.Contains(s, "missing \"job_id\""):
		return rejectFatal
	// Slot exhaustion (SI-55) - UNCONFIRMED wording; confirm against a live
	// over-slot rejection before extending this list.
	case strings.Contains(s, "no suitable queues"):
		return rejectSlots
	// Async execd race (SI-09): the job/task rejection GE emits before the slave
	// execd has registered the job (real: "didn't accept task").
	case strings.Contains(s, "didn't accept task"),
		strings.Contains(s, "does not exist"),
		strings.Contains(s, "can't find job"):
		return rejectJobRace
	default:
		// Unknown rejection: prefer a bounded retry over an immediate exit 8.
		return rejectJobRace
	}
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}

// qrshHandle wraps a launched qrsh child.
type qrshHandle struct {
	host string
	proc childProc
}

func (h *qrshHandle) Host() string { return h.host }
func (h *qrshHandle) Wait() error  { return h.proc.Wait() }
func (h *qrshHandle) Kill() error  { return h.proc.Kill() }

// spawnQrsh is the production childProc: it runs `qrsh <args>` with env, teeing
// the child's stderr to tee while also buffering a bounded tail for rejection
// classification.
func spawnQrsh(ctx context.Context, args, env []string, tee io.Writer) (childProc, error) {
	cmd := exec.CommandContext(ctx, gedata.ResolveCommand("qrsh"), args...)
	cmd.Env = env
	buf := &tailBuffer{limit: 8192}
	if tee != nil {
		cmd.Stderr = io.MultiWriter(tee, buf)
	} else {
		cmd.Stderr = buf
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd, stderr: buf, done: waitCh(cmd)}, nil
}

func waitCh(cmd *exec.Cmd) chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

type execProc struct {
	cmd    *exec.Cmd
	stderr *tailBuffer
	done   chan error

	mu      sync.Mutex
	waitErr error
	waited  bool
}

func (p *execProc) settle(d time.Duration) (bool, string) {
	select {
	case err := <-p.done:
		p.mu.Lock()
		p.waitErr, p.waited = err, true
		p.mu.Unlock()
		return true, p.stderr.String()
	case <-time.After(d):
		return false, ""
	}
}

func (p *execProc) Wait() error {
	p.mu.Lock()
	if p.waited {
		defer p.mu.Unlock()
		return p.waitErr
	}
	p.mu.Unlock()
	return <-p.done
}

func (p *execProc) Kill() error {
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

// tailBuffer keeps only the last `limit` bytes written, so a long-running
// stepper's stderr cannot grow unbounded in memory.
type tailBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, _ := t.buf.Write(p)
	if t.buf.Len() > t.limit {
		b := t.buf.Bytes()
		trimmed := append([]byte(nil), b[len(b)-t.limit:]...)
		t.buf.Reset()
		t.buf.Write(trimmed)
	}
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
