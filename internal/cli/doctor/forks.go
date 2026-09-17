package doctor

import (
	"fmt"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/launch"
)

// queueLimits is what one queue offering a PE contributes to the
// daemon_forks_slaves verdict: the per-slot memory limits it sets, or why they
// could not be read.
type queueLimits struct {
	Queue  string
	Limits []string
	Err    error
}

// forksFindings decides what doctor says about a PE's daemon_forks_slaves
// setting (SI-18, REQ-APX-003), given every queue that offers the PE.
//
// doctor used to print the tradeoff unconditionally, and for TRUE a second time
// through the preflight, so no setting of the switch could pass. The two sides
// are not symmetric, and srun's preflight already treats them so:
//
//	TRUE  -- concurrent srun steps will not run on a slave host. A functional
//	         limit whatever the queues set: one warning for the PE.
//	FALSE -- the installer's default. A stepper forking N ranks runs under one
//	         slot's rlimits, which can only hurt where a queue caps per-slot
//	         memory: a warning naming each such queue, otherwise a pass.
//
// A queue whose limits could not be read gets a warning too. Passing it would
// assert a safety doctor did not establish.
//
// The queue config is not the only source of a per-slot limit: when
// memory_complex is one of them, every --mem job requests it, and Grid Engine
// enforces that request as the queue instance's limit (reduce_queue_limit in
// sge_give_jobs.cc). That case warns even though every queue reads INFINITY.
func forksFindings(pe string, daemonForksSlaves bool, memoryComplex string, queues []queueLimits) (warns []string, pass string) {
	if daemonForksSlaves {
		return []string{launch.PEForksNote(true, pe)}, ""
	}
	if len(queues) == 0 {
		// No readable queue offers the PE; the wiring checks already failed
		// that, and there is nothing to judge FALSE against.
		return nil, ""
	}
	var checked []string
	for _, q := range queues {
		switch {
		case q.Err != nil:
			warns = append(warns, fmt.Sprintf("PE %q has daemon_forks_slaves FALSE, but queue %q "+
				"could not be read to check for per-slot memory limits (SI-18): %v", pe, q.Queue, q.Err))
		case len(q.Limits) > 0:
			warns = append(warns, fmt.Sprintf("%s; queue %q sets %s",
				launch.PEForksNote(false, pe), q.Queue, strings.Join(q.Limits, ", ")))
		default:
			checked = append(checked, q.Queue)
		}
	}
	if launch.IsPerSlotMemoryLimit(memoryComplex) {
		warns = append(warns, fmt.Sprintf("%s; memory_complex %s makes every --mem request a per-slot limit",
			launch.PEForksNote(false, pe), memoryComplex))
	}
	if len(warns) > 0 {
		return warns, ""
	}
	return nil, fmt.Sprintf("pe %s daemon_forks_slaves FALSE: concurrent srun steps can run, "+
		"and no queue offering it caps per-slot memory (%s)", pe, strings.Join(checked, ", "))
}
