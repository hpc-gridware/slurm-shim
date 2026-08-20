package gedata

import (
	"context"
	"fmt"
	"strings"
	"time"

	// go-clusterscheduler is the shared source of truth for OCS/GE command
	// formats; gedata is the shim's single boundary to it. qacct v9.1 re-exports
	// the v9.0 parser and the JobDetail type; the accounting fields sacct needs
	// are the stable classic ones, identical on OCS 9.0.10 and 9.1.4.
	qacct "github.com/hpc-gridware/go-clusterscheduler/pkg/qacct/v9.1"
)

// AcctQuery selects which finished jobs to report. A zero value asks for
// everything qacct will return, which is why sacct always sets at least one
// field.
type AcctQuery struct {
	JobID string // qacct -j
	User  string // qacct -o
	Begin string // qacct -b, CCYYMMDDhhmm
	End   string // qacct -e, CCYYMMDDhhmm
}

// AccountingRecord is one finished job or array task from qacct, adapted to the
// fields sacct reports. State and ExitCode are synthesized from GE's failed and
// exit_status; the rest are carried through as recorded.
type AccountingRecord struct {
	JobNumber int64
	TaskID    int64  // GE 1-based array task id; 0 for a non-array job
	State     string // synthesized SLURM state (COMPLETED/FAILED/CANCELLED/...)
	ExitCode  string // synthesized SLURM "code:signal"
	JobName   string
	User      string
	Queue     string // GE cluster queue, reported as the SLURM partition
	Account   string
	Host      string
	Slots     int64
	Submit    time.Time
	Start     time.Time
	End       time.Time
	Elapsed   time.Duration
	MaxRSS    float64 // bytes
	TotalCPU  time.Duration
}

// JobAccounting runs qacct through the runner and returns the finished records
// with a synthesized SLURM state. Parsing is delegated to go-clusterscheduler;
// this function only runs the command and adapts the result. A job that has not
// yet reached the accounting file (still running, or spooling) yields no
// records: qacct exits non-zero and prints nothing to stdout, which parses to an
// empty slice. That is deliberately not an error - callers treat "no record" as
// "unknown, keep polling".
func JobAccounting(ctx context.Context, runner Runner, q AcctQuery) ([]AccountingRecord, error) {
	args := qacctArgs(q)
	if len(args) == 0 {
		return nil, fmt.Errorf("accounting query needs at least one filter")
	}
	out, errOut, exit, err := runner.Run(ctx, "qacct", args...)
	if err != nil {
		return nil, err
	}
	// qacct exits 1 with empty stdout both for "job id not found" (benign: the
	// job has not reached the accounting file yet) and for a real failure such as
	// a rejected -b/-e bound, which prints a usage dump. Only the first is "no
	// records"; treating the second as an empty result would report "no jobs" for
	// a query that never actually ran.
	if exit != 0 && len(strings.TrimSpace(string(out))) == 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg != "" && !strings.Contains(msg, "not found") {
			return nil, fmt.Errorf("qacct exited %d: %s", exit, firstLine(msg))
		}
		return nil, nil
	}
	details, perr := qacct.ParseQAcctJobOutput(string(out))
	if perr != nil {
		return nil, perr
	}
	recs := make([]AccountingRecord, 0, len(details))
	for _, d := range details {
		// Skip PE task records. A cluster configured with the spec's recommended
		// accounting_summary FALSE (REQ-APX-004) writes one extra record per
		// `qrsh -inherit` task, all sharing the job's number and task id. Those
		// are steps, not jobs: sacct never emits step rows, and keeping them
		// would both duplicate the job and let a slave task's host and exit
		// status stand in for the job's own.
		if pt := strings.TrimSpace(d.PETaskID); pt != "" && pt != "NONE" {
			continue
		}
		rec := AccountingRecord{
			JobNumber: d.JobNumber,
			TaskID:    d.TaskID,
			State:     acctState(d.Failed, d.ExitStatus),
			ExitCode:  acctExitCode(d.Failed, d.ExitStatus),
			JobName:   d.JobName,
			User:      d.Owner,
			Queue:     d.QName,
			Account:   d.Account,
			Host:      d.HostName,
			Slots:     d.Slots,
			Submit:    geTime(d.SubmitTime),
			Start:     geTime(d.StartTime),
			End:       geTime(d.EndTime),
			MaxRSS:    d.JobUsage.Usage.MaxRSS,
			TotalCPU:  time.Duration((d.JobUsage.RUsage.RuUtime + d.JobUsage.RUsage.RuStime) * float64(time.Second)),
		}
		rec.Elapsed = elapsed(d.JobUsage.RUsage.RuWallclock, rec.Start, rec.End)
		recs = append(recs, rec)
	}
	return recs, nil
}

// qacctArgs renders the query as qacct flags, in a stable order so tests and
// recorded fixtures stay comparable.
//
// The leading -j is what puts qacct into per-job-record mode, and it is required
// even when no job id follows: "qacct -o <user>" on its own prints an aggregate
// usage summary for that owner (one line of totals), not the job records sacct
// needs. The owner and time filters then narrow that record listing.
func qacctArgs(q AcctQuery) []string {
	if q == (AcctQuery{}) {
		return nil
	}
	args := []string{"-j"}
	if q.JobID != "" {
		args = append(args, q.JobID)
	}
	if q.User != "" {
		args = append(args, "-o", q.User)
	}
	if q.Begin != "" {
		args = append(args, "-b", q.Begin)
	}
	if q.End != "" {
		args = append(args, "-e", q.End)
	}
	return args
}

// geTime converts a parsed qacct timestamp to a time. qacct prints wall-clock
// times in the cluster's own zone ("2026-08-20 19:44:47.425663") and
// go-clusterscheduler parses them as UTC before handing back microseconds, so
// reading them back as UTC reproduces exactly the digits qacct printed. Doing it
// any other way would shift every reported time by the shim host's UTC offset.
// A zero value means the field was absent.
func geTime(us int64) time.Time {
	if us <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

// elapsed prefers GE's ru_wallclock, the duration the execd actually measured,
// and falls back to end-start for a record written without it.
//
// end-start looks like the more precise choice and is how sacct computes
// Elapsed, but here it is the difference between two wall-clock LABELS rather
// than two instants (see geTime), so a job spanning a DST transition would be
// reported an hour long or an hour short. ru_wallclock has no such failure mode;
// its whole-second truncation is invisible at HH:MM:SS granularity.
func elapsed(wallclock int64, start, end time.Time) time.Duration {
	if wallclock > 0 {
		return time.Duration(wallclock) * time.Second
	}
	if !start.IsZero() && !end.IsZero() && end.After(start) {
		return end.Sub(start)
	}
	return 0
}

// firstLine returns s up to its first newline, for embedding a tool's error in a
// one-line message.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// acctState maps GE's qacct failed + exit_status to a terminal SLURM state.
// GE collapses CANCELLED, TIMEOUT and OUT_OF_MEMORY into failed=100,
// exit_status=137 for a started-then-killed job, and qacct alone cannot separate
// them without the requested h_rt/h_vmem, so failed=100 defaults to CANCELLED;
// the only distinct timeout signal is the qmaster-enforced failed=37. A consumer
// like submitit only needs *a* terminal state to stop waiting, so the
// CANCELLED/TIMEOUT nuance is cosmetic, not a correctness concern.
//
// This relies on go-clusterscheduler's qacct parser stripping the trailing
// description from the failed field (e.g. "100 : assumedly after job"); the
// pinned module does, so a killed job reports CANCELLED (not COMPLETED).
func acctState(failed, exit int64) string {
	switch {
	case failed == 0 && exit == 0:
		return "COMPLETED"
	case failed == 0:
		return "FAILED" // ran and exited non-zero (or died by a non-GE signal)
	case failed == 37:
		return "TIMEOUT" // qmaster-enforced h_rt
	case failed == 22:
		return "NODE_FAIL" // node / execd lost the job
	case failed == 24 || failed == 25:
		return "REQUEUED" // migrating / rescheduling (non-terminal; it runs again)
	case failed == 100:
		return "CANCELLED" // started then killed (qdel / limit); see doc comment
	default:
		return "FAILED" // setup / prolog / PE / epilog and other infra failures
	}
}

// acctExitCode renders SLURM's "code:signal" pair. GE reports one exit_status
// which is 128+signal when the job was signaled, so failed states that imply a
// kill are reported as a signal rather than a code, matching what sacct shows
// for the same outcome on SLURM.
func acctExitCode(failed, exit int64) string {
	switch {
	case failed == 0 && exit >= 128 && exit <= 255:
		return fmt.Sprintf("0:%d", exit-128) // died by a signal of its own accord
	case failed == 0:
		return fmt.Sprintf("%d:0", exit)
	case failed == 37 || failed == 100:
		return "0:9" // limit- or qdel-enforced SIGKILL
	default:
		return fmt.Sprintf("%d:0", failed) // infra failure: report GE's failed code
	}
}

// AcctActiveState maps a GE state code (from qstat) to the SLURM state sacct
// should report for a still-live job. It reuses the canonical MapState/FullState
// vocabulary and applies the one sacct-specific override: it never returns
// SUSPENDED (a consumer like submitit treats SUSPENDED as terminal and would stop
// polling), so GE-suspended jobs map to RUNNING. An empty state (which MapState
// would read as COMPLETED) is treated as PENDING, since a live qstat row always
// carries a state and reporting a terminal state here would wrongly stop polling.
func AcctActiveState(ge string) string {
	if strings.TrimSpace(ge) == "" {
		return "PENDING"
	}
	s := FullState(MapState(ge))
	if s == "SUSPENDED" {
		return "RUNNING"
	}
	return s
}
