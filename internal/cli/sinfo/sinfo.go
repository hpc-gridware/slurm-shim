// Package sinfo implements the sinfo shim (spec sec. 7.10): a partition listing
// derived from the config partitions map, with live node counts, states, and a
// compressed nodelist. The qstat -f data comes from gedata.QueueInstances (which
// parses via the go-clusterscheduler library); this package only maps GE queue
// states to SLURM node states and formats rows. --version is handled by the
// dispatcher. -h/--noheader and -o/--format are honoured; anything else is an
// error, because sinfo output is script-parsed and a silently dropped flag
// changes the shape of what the caller reads.
package sinfo

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// Run is the sinfo entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, warnings, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "sinfo: error: %v\n", err)
		return 2
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "sinfo: warning: %s\n", w)
	}
	return run(gedata.ExecRunner{}, cfg, args, stdout, stderr)
}

func run(runner gedata.Runner, cfg *config.Config, args []string, stdout, stderr io.Writer) int {
	opt, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "sinfo: error: %v\n", err)
		return 2
	}
	names := make([]string, 0, len(cfg.Partitions))
	for name := range cfg.Partitions {
		names = append(names, name)
	}
	sort.Strings(names)

	// -p/--partition. A name that does not exist is an error rather than an
	// empty listing: `sinfo -p gpu` returning nothing reads as "the partition is
	// empty" when it actually means "there is no such partition".
	if len(opt.partitions) > 0 {
		want := map[string]bool{}
		for _, p := range opt.partitions {
			if _, ok := cfg.Partitions[p]; !ok {
				fmt.Fprintf(stderr, "sinfo: error: invalid partition name %q\n", p)
				return 2
			}
			want[p] = true
		}
		kept := names[:0]
		for _, n := range names {
			if want[n] {
				kept = append(kept, n)
			}
		}
		names = kept
	}

	// Header LAST of the pre-flight steps: an invalid -p must not have already
	// written a header row to stdout before the error is reported.
	if !opt.noHeader && !opt.formatSet {
		fmt.Fprintln(stdout, "PARTITION AVAIL TIMELIMIT NODES STATE NODELIST")
	}

	// Live queue instances from GE. On failure sinfo degrades to a config-only
	// listing rather than erroring, so it stays useful off-cluster.
	instances, qErr := gedata.QueueInstances(context.Background(), runner)

	emit := func(f rowFields) int {
		if !opt.formatSet {
			fmt.Fprintf(stdout, "%s %s %s %s %s %s\n",
				f.partition, f.avail, f.timelimit, f.nodes, f.state, f.nodelist)
			return 0
		}
		line, ferr := renderFormat(opt.format, f)
		if ferr != nil {
			fmt.Fprintf(stderr, "sinfo: error: %v\n", ferr)
			return 2
		}
		fmt.Fprintln(stdout, line)
		return 0
	}

	for _, name := range names {
		rows := partitionRows(cfg.Partitions[name].Queue, instances)
		if len(rows) == 0 {
			// No live data (query failed, or the queue has no instances): keep the
			// neutral placeholder so the partition still shows up.
			if rc := emit(rowFields{name, "up", "infinite", "0", "n/a", "-"}); rc != 0 {
				return rc
			}
			continue
		}
		for _, r := range rows {
			if rc := emit(rowFields{
				name, "up", "infinite", fmt.Sprintf("%d", r.count), r.state, r.nodelist,
			}); rc != 0 {
				return rc
			}
		}
	}

	if qErr != nil {
		fmt.Fprintf(stderr, "sinfo: warning: could not query node states (%v); showing partitions only\n", qErr)
	}
	return 0
}

type sinfoRow struct {
	state    string
	count    int
	nodelist string
}

// partitionRows groups the instances of the partition's queue by SLURM node
// state and returns one row per state (sorted), each with a compressed nodelist.
func partitionRows(queue string, instances []gedata.QueueInstance) []sinfoRow {
	byState := map[string][]string{}
	for _, inst := range instances {
		if inst.Queue != queue {
			continue
		}
		st := nodeState(inst)
		byState[st] = append(byState[st], inst.Host)
	}
	states := make([]string, 0, len(byState))
	for st := range byState {
		states = append(states, st)
	}
	sort.Strings(states)

	rows := make([]sinfoRow, 0, len(states))
	for _, st := range states {
		hosts := byState[st]
		encoders.SortHosts(hosts) // numeric order so CompressNodelist ranges well
		rows = append(rows, sinfoRow{state: st, count: len(hosts), nodelist: encoders.CompressNodelist(hosts)})
	}
	return rows
}

// nodeState maps a GE queue-instance state to a coarse SLURM node state. GE
// letters: u=unreachable, E=error, c=config-ambiguous, o=orphaned -> down;
// d/D=disabled, s/S/C=suspended -> drain. Otherwise slot usage decides: full=
// allocated, partial=mix, empty=idle. (a/A=load/threshold alarm is not "down".)
func nodeState(inst gedata.QueueInstance) string {
	switch {
	case strings.ContainsAny(inst.States, "uEco"):
		return "down"
	case strings.ContainsAny(inst.States, "dDsSC"):
		return "drain"
	case inst.Total > 0 && inst.Used >= inst.Total:
		return "allocated"
	case inst.Used > 0:
		return "mix"
	default:
		return "idle"
	}
}
