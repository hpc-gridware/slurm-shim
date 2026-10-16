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

package sbatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// Polling intervals for --wait. The job is polled, never waited on with
// `qsub -sync y`: every -sync client holds one of qmaster's dynamic event
// clients for the whole run, and many concurrent waiters exhaust them. A poll
// is one short qstat; the interval grows so a long job costs few of them.
var (
	waitFirstPoll = 1 * time.Second
	waitMaxPoll   = 15 * time.Second
	// waitAcctFor bounds how long a finished job's accounting record may take
	// to appear (qacct is written a beat after the job leaves qstat).
	waitAcctFor = 2 * time.Minute
	// waitErrorsFor is how long qstat or qacct may keep failing before --wait
	// gives up: a qmaster restart or failover must not end a wait on a job
	// that runs for hours. waitCallTimeout bounds each call, so a hung client
	// cannot block the wait (or its interrupt) forever.
	waitErrorsFor   = 5 * time.Minute
	waitCallTimeout = 2 * time.Minute
)

// waitForJob blocks until job id has ended and returns the exit status
// sbatch --wait exits with: the job's exit status, the highest over an
// array's tasks (tasks is how many it has). A job killed or failed by Grid
// Engine exits non-zero even when it left no exit status of its own.
//
// An interrupt (SIGINT, SIGTERM, SIGHUP) stops the waiting, not the job: the
// job keeps its place, and sbatch exits 128+signal.
func waitForJob(runner gedata.Runner, id string, tasks int, stderr io.Writer, interrupt <-chan os.Signal) int {
	ctx := context.Background()
	versionCtx, cancel := context.WithTimeout(ctx, waitCallTimeout)
	b, err := gedata.OCSVersion(versionCtx, runner)
	cancel()
	if err == nil && !b.RecordsPEExitStatus() {
		fmt.Fprintf(stderr, "sbatch: warning: %s records 0 as the exit status of every parallel job "+
			"(fixed in OCS %s); --wait cannot see a failure\n", b, gedata.AccountingFixRelease)
	}

	// failing tolerates query errors until they have lasted waitErrorsFor; it
	// reports whether to give up.
	var failingSince time.Time
	failing := func(what string, err error) bool {
		if failingSince.IsZero() {
			failingSince = time.Now()
			fmt.Fprintf(stderr, "sbatch: warning: %s job %s: %v; still waiting\n", what, id, err)
		}
		if time.Since(failingSince) < waitErrorsFor {
			return false
		}
		fmt.Fprintf(stderr, "sbatch: error: %s job %s keeps failing: %v; stopped waiting, the job may still be running\n", what, id, err)
		return true
	}

	poll := waitFirstPoll
	for {
		callCtx, cancel := context.WithTimeout(ctx, waitCallTimeout)
		present, err := gedata.JobPresent(callCtx, runner, id)
		cancel()
		if err != nil {
			if failing("waiting for", err) {
				return 1
			}
		} else {
			failingSince = time.Time{}
			if !present {
				break
			}
		}
		if sig, stop := pause(poll, interrupt); stop {
			fmt.Fprintf(stderr, "sbatch: stopped waiting; job %s keeps running\n", id)
			return 128 + int(sig)
		}
		poll = min(poll*2, waitMaxPoll)
	}

	deadline := time.Now().Add(waitAcctFor)
	poll = waitFirstPoll
	for {
		callCtx, cancel := context.WithTimeout(ctx, waitCallTimeout)
		recs, err := gedata.JobAccounting(callCtx, runner, gedata.AcctQuery{JobID: id})
		cancel()
		if err != nil {
			if failing("reading the accounting record of", err) {
				return 1
			}
		} else {
			failingSince = time.Time{}
			if done := finishedTasks(recs); len(done) >= tasks {
				return highestExit(done)
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(stderr, "sbatch: error: job %s ended, but its accounting record did not appear; exit status unknown\n", id)
				return 1
			}
		}
		if sig, stop := pause(poll, interrupt); stop {
			fmt.Fprintf(stderr, "sbatch: stopped waiting for the accounting record of job %s\n", id)
			return 128 + int(sig)
		}
		poll = min(poll*2, waitMaxPoll)
	}
}

// pause sleeps for d unless an interrupt arrives first.
func pause(d time.Duration, interrupt <-chan os.Signal) (syscall.Signal, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case sig := <-interrupt:
		if s, ok := sig.(syscall.Signal); ok {
			return s, true
		}
		return syscall.SIGINT, true
	case <-t.C:
		return 0, false
	}
}

// finishedTasks keeps one record per task: a rescheduled task leaves a record
// per run, and only its last run counts.
func finishedTasks(recs []gedata.AccountingRecord) map[int64]gedata.AccountingRecord {
	out := map[int64]gedata.AccountingRecord{}
	for _, r := range recs {
		if prev, ok := out[r.TaskID]; !ok || r.End.After(prev.End) {
			out[r.TaskID] = r
		}
	}
	return out
}

// highestExit is the highest process exit code over the records. Grid Engine's
// exit_status is already 128+signal for a job killed by a signal; a job Grid
// Engine failed or killed without one (failed != 0, exit_status 0) counts as 1.
func highestExit(recs map[int64]gedata.AccountingRecord) int {
	worst := 0
	for _, r := range recs {
		code := int(r.ExitStatus)
		if r.Failed != 0 && code == 0 {
			code = 1
		}
		worst = max(worst, code)
	}
	return worst
}

// interruptSignals delivers the signals that stop sbatch --wait.
func interruptSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return ch, func() { signal.Stop(ch) }
}
