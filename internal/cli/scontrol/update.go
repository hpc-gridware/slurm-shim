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

package scontrol

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// jobUpdate is a validated `scontrol update job` request.
type jobUpdate struct {
	jobID     string
	name      string // "" leaves the name alone
	timeSet   bool
	unlimited bool
	seconds   int
}

// updateCmd changes a job's TimeLimit and/or JobName with one qalter call.
// Every field is checked before anything runs, so a typo in the second field
// does not leave the first one applied.
func updateCmd(runner gedata.Runner, args []string, stderr io.Writer) int {
	u, err := parseJobUpdate(args)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: %v\n", err)
		return 1
	}
	qalterArgs := []string{}
	if u.name != "" {
		qalterArgs = append(qalterArgs, "-N", u.name)
	}
	if u.timeSet {
		// qalter -l REPLACES the job's whole global hard resource list, so the
		// current list is read and only h_rt (and a --signal s_rt) changed;
		// memory, GPU and license requests survive. Verified on OCS 9.1.6.
		detail, found, err := gedata.FetchJobDetail(context.Background(), runner, u.jobID, "")
		if err != nil {
			fmt.Fprintf(stderr, "scontrol: error: reading job %s: %v\n", u.jobID, err)
			return 1
		}
		if !found {
			fmt.Fprintln(stderr, "scontrol: error: Invalid job id specified")
			return 1
		}
		// execd enforces the h_rt it was given at dispatch: qalter changes only
		// qmaster's record, so a started job would still be killed at its old
		// limit while show job printed the new one (verified on OCS 9.1.6).
		if len(detail.Starts) > 0 {
			fmt.Fprintf(stderr, "scontrol: error: job %s has started: Grid Engine fixes the time limit "+
				"when a job starts, so TimeLimit can only be changed while it is pending\n", u.jobID)
			return 1
		}
		merged := mergeTimeLimit(detail.HardResources, u.seconds, u.unlimited)
		qalterArgs = append(qalterArgs, "-l", gedata.ResourceList(merged))
	}
	qalterArgs = append(qalterArgs, u.jobID)
	out, errOut, exit, err := runner.Run(context.Background(), "qalter", qalterArgs...)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: running qalter: %v\n", err)
		return 1
	}
	if exit != 0 {
		fmt.Fprintf(stderr, "scontrol: error: %s\n", geMessage(out, errOut, "qalter failed"))
		return 1
	}
	return 0
}

// parseJobUpdate reads "JobId=<id> Key=Value ...". Keys are case-insensitive,
// as in SLURM; "job=" and "jobid=" both name the job.
func parseJobUpdate(args []string) (jobUpdate, error) {
	var u jobUpdate
	for _, a := range args {
		key, value, ok := strings.Cut(a, "=")
		if !ok {
			return u, fmt.Errorf("invalid input: %s (expected Key=Value)", a)
		}
		switch strings.ToLower(key) {
		case "job", "jobid":
			u.jobID = value
		case "jobname", "name":
			if strings.TrimSpace(value) == "" {
				return u, fmt.Errorf("invalid JobName value: %q", value)
			}
			u.name = value
		case "timelimit":
			secs, unlimited, err := parseTimeLimit(value)
			if err != nil {
				return u, err
			}
			u.timeSet, u.seconds, u.unlimited = true, secs, unlimited
		case "partition":
			// A partition is a queue plus a PE and a slot rule, all applied at
			// submission; moving only the queue would leave a mixed job.
			return u, fmt.Errorf("update of Partition is not supported (resubmit to the other partition)")
		case "numnodes":
			return u, fmt.Errorf("update of NumNodes is not supported (the node count is fixed by the job's parallel environment request)")
		default:
			return u, fmt.Errorf("update of %s is not supported (supported: TimeLimit, JobName)", key)
		}
	}
	if u.jobID == "" {
		return u, fmt.Errorf("no job specified (use JobId=<id>)")
	}
	if strings.Contains(u.jobID, "_") {
		// qalter changes the whole array job; silently widening a task-scoped
		// update to every task would change tasks the user did not name.
		return u, fmt.Errorf("update of a single array task (%s) is not supported; name the array job", u.jobID)
	}
	if n, err := strconv.Atoi(u.jobID); err != nil || n <= 0 {
		return u, fmt.Errorf("invalid job id specified: %s", u.jobID)
	}
	if u.name == "" && !u.timeSet {
		return u, fmt.Errorf("nothing to update for job %s", u.jobID)
	}
	return u, nil
}

// parseTimeLimit reads a SLURM time limit: minutes, MM:SS, HH:MM:SS,
// D-HH[:MM[:SS]], or UNLIMITED/INFINITE. The relative forms (+N/-N) are not
// supported: the new limit is computed from the current one, and an absolute
// value says exactly what the job gets.
func parseTimeLimit(v string) (seconds int, unlimited bool, err error) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "UNLIMITED", "INFINITE":
		return 0, true, nil
	}
	if strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-") {
		return 0, false, fmt.Errorf("invalid TimeLimit value: %s (relative limits are not supported; give the new limit)", v)
	}
	if v == "" || strings.Trim(v, "0123456789:-") != "" || strings.Count(v, ":") > 2 || strings.Count(v, "-") > 1 {
		return 0, false, fmt.Errorf("invalid TimeLimit value: %s", v)
	}
	secs, err := submit.ParseSlurmTime(v)
	if err != nil || secs <= 0 {
		return 0, false, fmt.Errorf("invalid TimeLimit value: %s", v)
	}
	return secs, false, nil
}

// mergeTimeLimit returns res with h_rt set to seconds, or removed for an
// unlimited job. A soft limit below the old hard one is sbatch --signal's
// warning time (s_rt = h_rt - delay): it keeps the same distance to the new
// limit, and goes when the job becomes unlimited or the gap no longer fits.
// Every other request is kept in its place.
func mergeTimeLimit(res []gedata.Resource, seconds int, unlimited bool) []gedata.Resource {
	oldHard, hasHard := lookupSeconds(res, "h_rt")
	oldSoft, hasSoft := lookupSeconds(res, "s_rt")
	signalGap := 0
	if hasHard && hasSoft && oldSoft < oldHard {
		signalGap = oldHard - oldSoft
	}
	var out []gedata.Resource
	placed := false
	for _, r := range res {
		switch {
		case r.Name == "h_rt":
			if !unlimited {
				out = append(out, gedata.Resource{Name: "h_rt", Value: strconv.Itoa(seconds)})
				placed = true
			}
		case r.Name == "s_rt" && signalGap > 0:
			if !unlimited && seconds > signalGap {
				out = append(out, gedata.Resource{Name: "s_rt", Value: strconv.Itoa(seconds - signalGap)})
			}
		default:
			out = append(out, r)
		}
	}
	if !unlimited && !placed {
		out = append(out, gedata.Resource{Name: "h_rt", Value: strconv.Itoa(seconds)})
	}
	return out
}

// lookupSeconds reads a TIME request as seconds; INFINITY or an unreadable
// value counts as absent.
func lookupSeconds(res []gedata.Resource, name string) (int, bool) {
	for _, r := range res {
		if r.Name == name {
			return geSeconds(r.Value)
		}
	}
	return 0, false
}

// geSeconds reads a GE TIME value as seconds; ok is false for INFINITY and
// anything unreadable.
func geSeconds(v string) (int, bool) {
	secs, unlimited, ok := gedata.ParseTime(v)
	if !ok || unlimited {
		return 0, false
	}
	return int(secs), true
}
