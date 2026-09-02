// Package squeue implements the squeue shim (spec sec. 7.7): it queries qstat,
// maps GE states to SLURM states, and renders the default 8-column format or a
// user -o/--format string.
package squeue

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// defaultFormat is SLURM's default squeue output (verified vs squeue.c):
// JOBID PARTITION NAME USER ST TIME NODES NODELIST(REASON) (REQ-SQU-002).
const defaultFormat = "%.18i %.9P %.8j %.8u %.2t %.10M %.6D %R"

type options struct {
	jobID    string
	user     string
	noHeader bool
	format   string
}

// Run is the squeue entry point. The config error is surfaced rather than
// discarded: config.Parse returns a nil *Config on a hard error, so swallowing it
// turns a malformed config into a nil dereference instead of a diagnostic.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, warnings, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "squeue: error: loading config: %v\n", err)
		return 1
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "squeue: warning: %s\n", w)
	}
	return run(gedata.ExecRunner{}, cfg, args, stdout, stderr)
}

func run(runner gedata.Runner, cfg *config.Config, args []string, stdout, stderr io.Writer) int {
	opt := parse(args)
	if opt.format == "" {
		opt.format = defaultFormat
	}

	qargs := []string{"-xml"}
	if opt.user != "" {
		qargs = append(qargs, "-u", opt.user)
	} else {
		qargs = append(qargs, "-u", "*")
	}

	out, errOut, exit, err := runner.Run(context.Background(), "qstat", qargs...)
	if err != nil {
		fmt.Fprintf(stderr, "squeue: error: running qstat: %v\n", err)
		return 1
	}
	if exit != 0 {
		if msg := strings.TrimSpace(string(errOut)); msg != "" {
			fmt.Fprintf(stderr, "squeue: error: %s\n", msg)
		}
		return 1
	}

	rows, err := gedata.ParseQstatXML(out)
	if err != nil {
		fmt.Fprintf(stderr, "squeue: error: parsing qstat output: %v\n", err)
		return 1
	}
	if opt.jobID != "" {
		rows = filterByJob(rows, opt.jobID)
	}

	if !opt.noHeader {
		fmt.Fprintln(stdout, formatHeader(opt.format))
	}
	for _, r := range rows {
		fmt.Fprintln(stdout, formatRow(opt.format, r, cfg))
	}
	return 0
}

func parse(args []string) options {
	var opt options
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--noheader":
			opt.noHeader = true
		case a == "-j" || a == "--jobs":
			if i++; i < len(args) {
				opt.jobID = args[i]
			}
		case strings.HasPrefix(a, "--jobs="):
			opt.jobID = a[len("--jobs="):]
		case strings.HasPrefix(a, "-j") && len(a) > 2:
			opt.jobID = a[2:]
		case a == "-u" || a == "--user":
			if i++; i < len(args) {
				opt.user = args[i]
			}
		case strings.HasPrefix(a, "--user="):
			opt.user = a[len("--user="):]
		case strings.HasPrefix(a, "-u") && len(a) > 2:
			opt.user = a[2:]
		case a == "-o" || a == "--format":
			if i++; i < len(args) {
				opt.format = args[i]
			}
		case strings.HasPrefix(a, "--format="):
			opt.format = a[len("--format="):]
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			opt.format = a[2:]
		}
	}
	return opt
}

// filterByJob keeps rows matching a SLURM job id, including the array form
// "4711_2" (REQ-SQU-002).
func filterByJob(rows []gedata.JobRow, jobID string) []gedata.JobRow {
	baseID, taskID := jobID, ""
	if i := strings.IndexByte(jobID, '_'); i >= 0 {
		baseID, taskID = jobID[:i], jobID[i+1:]
	}
	var out []gedata.JobRow
	for _, r := range rows {
		if r.JobID != baseID {
			continue
		}
		if taskID != "" && r.TaskID != taskID {
			continue
		}
		out = append(out, r)
	}
	return out
}
