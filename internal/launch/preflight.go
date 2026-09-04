package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// PreflightResult reports what a launch preflight found. Errors are fail-loud
// (srun must abort before launching); Warnings are advisory.
type PreflightResult struct {
	Errors   []string
	Warnings []string
}

// OK reports whether the preflight found no fail-loud errors.
func (r PreflightResult) OK() bool { return len(r.Errors) == 0 }

// Preflight validates that the allocation's parallel environment can host
// `qrsh -inherit` tight integration and surfaces the per-slot rlimit hazards
// (REQ-CHN-005, SI-18). peName is the job's PE; when empty (single-node local
// jobs) the checks are skipped.
func Preflight(ctx context.Context, r gedata.Runner, peName string) PreflightResult {
	var res PreflightResult
	if peName == "" || r == nil {
		return res
	}

	pe, err := PEConfig(ctx, r, peName)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("cannot read PE %q config: %v", peName, err))
		return res
	}

	// control_slaves TRUE is mandatory: without it the slave execd will not
	// accept `qrsh -inherit` tasks, so tight-integration launch is impossible.
	if !strings.EqualFold(pe["control_slaves"], "TRUE") {
		res.Errors = append(res.Errors, fmt.Sprintf(
			"PE %q has control_slaves=%q; qrsh -inherit tight integration requires control_slaves TRUE",
			peName, pe["control_slaves"]))
	}

	// Per-slot rlimit hazard (SI-18, REQ-APX-003). daemon_forks_slaves trades
	// concurrent steps for multiplied rlimits; its default (FALSE) leaves a
	// multi-rank stepper under one slot's rlimits, risking OOM under per-slot
	// h_vmem.
	switch {
	case strings.EqualFold(pe["daemon_forks_slaves"], "TRUE"):
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"PE %q has daemon_forks_slaves TRUE: per-slot rlimits are multiplied by slot count, but execd is capped to one task per slave host (concurrent srun steps will not run)", peName))
	default:
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"PE %q has daemon_forks_slaves FALSE: a stepper forking N ranks runs under one slot's rlimits, so per-slot h_vmem limits can OOM multi-rank steps (SI-18)", peName))
	}

	// Token delivery (SI-51, REQ-CHN-005): the per-step token travels via
	// `qrsh -v`, which GE stages in the execd spool. tokenSpoolWarning checks
	// whether that staging area is reachable by other users and stays silent
	// when it is not, so this is a finding rather than a standing reminder.
	if w := tokenSpoolWarning(ctx, r); w != "" {
		res.Warnings = append(res.Warnings, w)
	}

	return res
}

// PEConfig reads a parallel environment's configuration with `qconf -sp`. It is
// the single reader of that command, so callers that need PE facts (the launch
// preflight, the sbatch dry run) cannot disagree about how a failure is detected.
//
// A non-zero exit with empty stderr still yields an error: an empty message read
// as success is how a failed lookup gets reported as a definitive verdict.
func PEConfig(ctx context.Context, r gedata.Runner, peName string) (map[string]string, error) {
	stdout, stderr, exit, err := r.Run(ctx, "qconf", "-sp", peName)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = fmt.Sprintf("qconf -sp %s exited %d", peName, exit)
		}
		return nil, errors.New(msg)
	}
	return ParsePEConfig(stdout), nil
}

// ParsePEConfig parses `qconf -sp <pe>` output (one "key   value" pair per line)
// into a map. Values are the remainder of the line after the first run of
// whitespace, trimmed.
func ParsePEConfig(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		out[fields[0]] = strings.Join(fields[1:], " ")
	}
	return out
}
