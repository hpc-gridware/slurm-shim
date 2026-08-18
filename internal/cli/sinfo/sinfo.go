// Package sinfo implements the sinfo shim (spec sec. 7.10): a partition listing
// derived from the config partitions map, with live node counts, states, and a
// compressed nodelist. The qstat -f data comes from gedata.QueueInstances (which
// parses via the go-clusterscheduler library); this package only maps GE queue
// states to SLURM node states and formats rows. --version is handled by the
// dispatcher; other flags are ignored (SI-32).
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
	_ = args // no flags beyond --version (handled upstream) are supported (SI-32)
	fmt.Fprintln(stdout, "PARTITION AVAIL TIMELIMIT NODES STATE NODELIST")

	// Live queue instances from GE. On failure sinfo degrades to a config-only
	// listing rather than erroring, so it stays useful off-cluster.
	instances, qErr := gedata.QueueInstances(context.Background(), runner)

	names := make([]string, 0, len(cfg.Partitions))
	for name := range cfg.Partitions {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rows := partitionRows(cfg.Partitions[name].Queue, instances)
		if len(rows) == 0 {
			// No live data (query failed, or the queue has no instances): keep the
			// neutral placeholder so the partition still shows up.
			fmt.Fprintf(stdout, "%s up infinite 0 n/a -\n", name)
			continue
		}
		for _, r := range rows {
			fmt.Fprintf(stdout, "%s up infinite %d %s %s\n", name, r.count, r.state, r.nodelist)
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
