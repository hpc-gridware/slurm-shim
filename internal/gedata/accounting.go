package gedata

import (
	"context"
	"strings"

	// go-clusterscheduler is the shared source of truth for OCS/GE command
	// formats; gedata is the shim's single boundary to it. qacct v9.1 re-exports
	// the v9.0 parser and the JobDetail type; the accounting fields sacct needs
	// (job/task number, failed, exit_status) are the stable classic ones,
	// identical on OCS 9.0.10 and 9.1.4.
	qacct "github.com/hpc-gridware/go-clusterscheduler/pkg/qacct/v9.1"
)

// AccountingRecord is one finished job or array task from qacct, adapted to the
// shim's needs. State is a SLURM state synthesized from GE's failed/exit_status.
type AccountingRecord struct {
	JobNumber int64
	TaskID    int64  // GE 1-based array task id; 0 for a non-array job
	State     string // synthesized SLURM state (COMPLETED/FAILED/CANCELLED/...)
}

// JobAccounting runs `qacct -j <jobID>` through the runner and returns the
// finished records with a synthesized SLURM state. Parsing is delegated to
// go-clusterscheduler; this function only runs the command and adapts the
// result. A job that has not yet reached the accounting file (still running, or
// spooling) yields no records: qacct exits non-zero and prints nothing to
// stdout, which parses to an empty slice. That is deliberately not an error -
// callers treat "no record" as "unknown, keep polling".
func JobAccounting(ctx context.Context, runner Runner, jobID string) ([]AccountingRecord, error) {
	out, _, _, err := runner.Run(ctx, "qacct", "-j", jobID)
	if err != nil {
		return nil, err
	}
	details, perr := qacct.ParseQAcctJobOutput(string(out))
	if perr != nil {
		// Empty "job id not found" output parses cleanly to nothing; only a real
		// parse error on non-empty output is worth surfacing.
		if len(strings.TrimSpace(string(out))) == 0 {
			return nil, nil
		}
		return nil, perr
	}
	recs := make([]AccountingRecord, 0, len(details))
	for _, d := range details {
		recs = append(recs, AccountingRecord{
			JobNumber: d.JobNumber,
			TaskID:    d.TaskID,
			State:     acctState(d.Failed, d.ExitStatus),
		})
	}
	return recs, nil
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
