// Package scontrol implements the scontrol shim (spec sec. 7.9): hostlist
// expansion, a minimal show job, and task-scoped requeue.
package scontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// Run is the scontrol entry point. Under SLURM_SHIM_DRY_RUN the runner reports the
// mutating clients (requeue's qmod) on stderr instead of running them; the
// read-only qstat behind `show job` still runs, so show subcommands work as usual
// and their key=value record keeps stdout to itself.
func Run(args []string, stdout, stderr io.Writer) int {
	if dryrun.Enabled() {
		fmt.Fprintln(stderr, dryrun.Banner("scontrol"))
	}
	return run(dryrun.Wrap(gedata.ExecRunner{}, stderr, "scontrol"), args, stdout, stderr)
}

func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: no subcommand given")
		return 1
	}
	switch args[0] {
	case "show":
		return showCmd(runner, args[1:], stdout, stderr)
	case "requeue":
		return requeueCmd(runner, args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "scontrol: error: unsupported subcommand %q\n", args[0])
		return 1
	}
}

func showCmd(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: show needs an entity")
		return 1
	}
	switch args[0] {
	case "hostnames", "hostname":
		return showHostnames(args[1:], stdout, stderr)
	case "job":
		return showJob(runner, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "scontrol: error: unsupported show entity %q\n", args[0])
		return 1
	}
}

// showHostnames expands a nodelist to one hostname per line (REQ-SCT-001). With
// no argument it expands $SLURM_JOB_NODELIST (then $SLURM_NODELIST).
func showHostnames(args []string, stdout, stderr io.Writer) int {
	list := ""
	if len(args) > 0 {
		list = args[0]
	} else if v := os.Getenv("SLURM_JOB_NODELIST"); v != "" {
		list = v
	} else {
		list = os.Getenv("SLURM_NODELIST")
	}
	if list == "" {
		return 0 // nothing to expand
	}
	hosts, err := encoders.ExpandNodelist(list)
	if err != nil {
		fmt.Fprintln(stderr, "scontrol: error: Invalid hostlist")
		return 1
	}
	for _, h := range hosts {
		fmt.Fprintln(stdout, h)
	}
	return 0
}

// showJob prints a minimal key=value record for a job. Inside a running job the
// fabricated layout is used (rich, allocation-ordered nodelist, REQ-SCT-003);
// otherwise (or for a different job id from a login shell) the job is looked up
// in GE via qstat.
func showJob(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	id := ""
	if len(args) > 0 {
		id = args[0]
	}

	// Prefer the fabricated layout when it is present and describes the requested
	// job (or no id was given: "show my job").
	lay, layErr := loadLayout()
	if layErr == nil && (id == "" || matchesLayout(id, lay)) {
		return renderLayoutJob(lay, stdout)
	}
	if id == "" {
		// With no id we can only show "my" job from the in-job layout. A layout
		// that exists but failed to read is a real error, not "not inside a job".
		if layErr != nil && !errors.Is(layErr, os.ErrNotExist) {
			fmt.Fprintf(stderr, "scontrol: error: %v\n", layErr)
			return 1
		}
		fmt.Fprintln(stderr, "scontrol: error: no job id specified (and not inside a job)")
		return 1
	}

	// Look the job up in GE. Array-task ids (4711_2) match the base job number and,
	// when a task is given, that specific task (same rule as squeue, REQ-SQU-002).
	base, task, _ := strings.Cut(id, "_")
	out, errOut, exit, err := runner.Run(context.Background(), "qstat", "-xml", "-u", "*")
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: running qstat: %v\n", err)
		return 1
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = "qstat failed"
		}
		fmt.Fprintf(stderr, "scontrol: error: %s\n", msg)
		return 1
	}
	rows, err := gedata.ParseQstatXML(out)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: parsing qstat output: %v\n", err)
		return 1
	}
	for _, r := range rows {
		if r.JobID != base || (task != "" && r.TaskID != task) {
			continue
		}
		return renderGEJob(id, r, stdout)
	}
	fmt.Fprintln(stderr, "scontrol: error: Invalid job id specified")
	return 1
}

// matchesLayout reports whether id (possibly an array-task id) names the job the
// fabricated layout describes.
func matchesLayout(id string, lay *layout.Layout) bool {
	base, _, _ := strings.Cut(id, "_")
	return base == strconv.FormatInt(lay.Job.JobID, 10)
}

func renderLayoutJob(lay *layout.Layout, stdout io.Writer) int {
	hosts := make([]string, len(lay.Nodes))
	for i, n := range lay.Nodes {
		hosts[i] = n.Host
	}
	printFields(stdout, [][2]string{
		{"JobId", strconv.FormatInt(lay.Job.JobID, 10)},
		{"JobName", lay.Job.Name},
		{"JobState", "RUNNING"},
		{"NodeList", encoders.CompressNodelist(hosts)},
		{"NumNodes", strconv.Itoa(len(lay.Nodes))},
		{"NumTasks", strconv.Itoa(lay.Tasks.NTasks)},
		{"Partition", lay.Job.Partition},
		{"UserId", fmt.Sprintf("%s(%d)", lay.Job.User, lay.Job.UID)},
		{"WorkDir", lay.Job.SubmitDir},
	})
	return 0
}

// renderGEJob renders the minimal record available from `qstat -xml`: the master
// queue instance gives the partition (queue) and node; a pending job has none.
func renderGEJob(id string, r gedata.JobRow, stdout io.Writer) int {
	queue, host, _ := strings.Cut(r.Queue, "@")
	nodelist := host
	if nodelist == "" {
		nodelist = "(null)" // not yet scheduled onto a node
	}
	printFields(stdout, [][2]string{
		{"JobId", id},
		{"JobName", r.Name},
		{"JobState", gedata.FullState(gedata.MapState(r.State))},
		{"NodeList", nodelist},
		{"NumTasks", strconv.Itoa(r.Slots)},
		{"Partition", queue},
		{"UserId", r.User},
	})
	return 0
}

func printFields(stdout io.Writer, fields [][2]string) {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f[0] + "=" + f[1]
	}
	fmt.Fprintln(stdout, strings.Join(parts, " "))
}

// requeueCmd maps a SLURM job (or array-task) id to a GE reschedule (REQ-SCT-002).
// The compound array form "4711_2" targets only that task as "4711.2".
func requeueCmd(runner gedata.Runner, args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: requeue needs a job id")
		return 1
	}
	target := args[0]
	if i := strings.IndexByte(target, '_'); i >= 0 {
		target = target[:i] + "." + target[i+1:] // 4711_2 -> 4711.2
	}
	_, errOut, exit, err := runner.Run(context.Background(), "qmod", "-rj", target)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: running qmod: %v\n", err)
		return 1
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = "qmod refused the reschedule (rerun may be disabled for this queue)"
		}
		fmt.Fprintf(stderr, "scontrol: error: %s\n", msg)
		return 1
	}
	return 0
}

// loadLayout reads the in-job layout. An unset TMPDIR is reported as
// os.ErrNotExist -- "not inside a job" -- rather than falling back to a shared
// /tmp path a co-tenant could have planted a layout in (REQ-FAB-010). The
// caller already treats that error as the not-in-a-job case and falls through
// to the qstat lookup.
func loadLayout() (*layout.Layout, error) {
	dir, err := layout.StateDirFor(os.Getenv("TMPDIR"))
	if err != nil {
		return nil, err
	}
	return layout.Read(filepath.Join(dir, layout.LayoutFile))
}
