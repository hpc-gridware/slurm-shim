// Package scontrol implements the scontrol shim (spec sec. 7.9): hostlist
// expansion, a minimal show job, and task-scoped requeue.
package scontrol

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// Run is the scontrol entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(gedata.ExecRunner{}, args, stdout, stderr)
}

func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: no subcommand given")
		return 1
	}
	switch args[0] {
	case "show":
		return showCmd(args[1:], stdout, stderr)
	case "requeue":
		return requeueCmd(runner, args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "scontrol: error: unsupported subcommand %q\n", args[0])
		return 1
	}
}

func showCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: show needs an entity")
		return 1
	}
	switch args[0] {
	case "hostnames", "hostname":
		return showHostnames(args[1:], stdout, stderr)
	case "job":
		return showJob(args[1:], stdout, stderr)
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

// showJob prints a minimal key=value record sourced from the layout, with the
// node list rendered from the layout so its order matches the allocation
// (REQ-SCT-003).
func showJob(args []string, stdout, stderr io.Writer) int {
	lay, err := loadLayout()
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: %v\n", err)
		return 1
	}
	hosts := make([]string, len(lay.Nodes))
	for i, n := range lay.Nodes {
		hosts[i] = n.Host
	}
	fields := [][2]string{
		{"JobId", strconv.FormatInt(lay.Job.JobID, 10)},
		{"JobName", lay.Job.Name},
		{"JobState", "RUNNING"},
		{"NodeList", encoders.CompressNodelist(hosts)},
		{"NumNodes", strconv.Itoa(len(lay.Nodes))},
		{"NumTasks", strconv.Itoa(lay.Tasks.NTasks)},
		{"Partition", lay.Job.Partition},
		{"UserId", fmt.Sprintf("%s(%d)", lay.Job.User, lay.Job.UID)},
		{"WorkDir", lay.Job.SubmitDir},
	}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f[0] + "=" + f[1]
	}
	fmt.Fprintln(stdout, strings.Join(parts, " "))
	_ = args
	return 0
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

func loadLayout() (*layout.Layout, error) {
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	return layout.Read(filepath.Join(tmp, layout.StateDir, layout.LayoutFile))
}
