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

// Package scontrol implements the scontrol shim (spec sec. 7.9): hostlist
// expansion, show job/node/partition, job hold/release, suspend/resume, a
// subset of update job, and task-scoped requeue.
package scontrol

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// Run is the scontrol entry point. Under SLURM_SHIM_DRY_RUN the runner reports the
// mutating clients (qmod, qhold, qrls, qalter) on stderr instead of running them;
// the read-only qstat/qhost/qconf behind the show subcommands still run, so show
// works as usual and its records keep stdout to themselves.
func Run(args []string, stdout, stderr io.Writer) int {
	if dryrun.Enabled() {
		fmt.Fprintln(stderr, dryrun.Banner("scontrol"))
	}
	// A broken config only matters to the subcommands that read it: show
	// hostnames, hold or requeue must keep working, so fall back to the defaults
	// and let show partition, which has nothing to show without it, fail.
	cfg, warnings, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: warning: config: %v; using defaults\n", err)
		cfg = nil
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "scontrol: warning: %s\n", w)
	}
	return run(dryrun.Wrap(gedata.ExecRunner{}, stderr, "scontrol"), cfg, args, stdout, stderr)
}

// run dispatches a subcommand. cfg is nil when the config could not be loaded.
func run(runner gedata.Runner, cfg *config.Config, args []string, stdout, stderr io.Writer) int {
	args, oneliner := stripOneliner(args)
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: no subcommand given")
		return 1
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "show":
		return showCmd(runner, cfg, rest, oneliner, stdout, stderr)
	case "requeue":
		return requeueCmd(runner, rest, stderr)
	case "hold", "uhold":
		return holdCmd(runner, "qhold", rest, stderr)
	case "release":
		return holdCmd(runner, "qrls", rest, stderr)
	case "suspend":
		return suspendCmd(runner, "-sj", rest, stderr)
	case "resume":
		return suspendCmd(runner, "-usj", rest, stderr)
	case "update":
		return updateCmd(runner, rest, stderr)
	default:
		fmt.Fprintf(stderr, "scontrol: error: unsupported subcommand %q\n", cmd)
		return 1
	}
}

// stripOneliner removes -o/--oneliner wherever it appears: SLURM accepts it
// before the subcommand, and scripts also write `scontrol show -o job`. No job
// id, node or partition name starts with a dash, so nothing else is lost.
func stripOneliner(args []string) ([]string, bool) {
	var kept []string
	oneliner := false
	for _, a := range args {
		if a == "-o" || a == "--oneliner" {
			oneliner = true
			continue
		}
		kept = append(kept, a)
	}
	return kept, oneliner
}

func showCmd(runner gedata.Runner, cfg *config.Config, args []string, oneliner bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: show needs an entity")
		return 1
	}
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	switch strings.ToLower(args[0]) {
	case "hostnames", "hostname":
		return showHostnames(args[1:], stdout, stderr)
	case "job", "jobs", "jobid":
		return showJob(runner, orDefault(cfg), name, oneliner, stdout, stderr)
	case "node", "nodes", "nodename":
		return showNodes(runner, orDefault(cfg), name, oneliner, stdout, stderr)
	case "partition", "partitions", "partitionname":
		if cfg == nil {
			fmt.Fprintln(stderr, "scontrol: error: no partitions: the slurm-shim config could not be loaded")
			return 1
		}
		return showPartitions(runner, cfg, name, oneliner, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "scontrol: error: unsupported show entity %q\n", args[0])
		return 1
	}
}

func orDefault(cfg *config.Config) *config.Config {
	if cfg == nil {
		return config.Default()
	}
	return cfg
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

// field is one Key=Value of a record; a record is its fields grouped into the
// lines SLURM prints them on.
type field struct{ key, value string }

type record [][]field

// printRecord writes a record the way scontrol show does: the first line flush,
// the rest indented by three spaces, a blank line after the record. With
// -o/--oneliner the whole record is one space-separated line.
func printRecord(w io.Writer, rec record, oneliner bool) {
	if oneliner {
		var parts []string
		for _, line := range rec {
			for _, f := range line {
				parts = append(parts, f.key+"="+f.value)
			}
		}
		fmt.Fprintln(w, strings.Join(parts, " "))
		return
	}
	for i, line := range rec {
		if len(line) == 0 {
			continue
		}
		parts := make([]string, len(line))
		for j, f := range line {
			parts[j] = f.key + "=" + f.value
		}
		indent := "   "
		if i == 0 {
			indent = ""
		}
		fmt.Fprintln(w, indent+strings.Join(parts, " "))
	}
	fmt.Fprintln(w)
}

// requeueCmd maps a SLURM job (or array-task) id to a GE reschedule (REQ-SCT-002).
// The compound array form "4711_2" targets only that task: SLURM index 2 is GE
// task 3, so "4711.3" (SI-14) -- the same element scancel 4711_2 cancels.
func requeueCmd(runner gedata.Runner, args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: requeue needs a job id")
		return 1
	}
	target, ok := geTarget(args[0])
	if !ok {
		fmt.Fprintf(stderr, "scontrol: error: invalid job id %s\n", args[0])
		return 1
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
