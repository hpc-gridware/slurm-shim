package scontrol

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// showNodes prints one record per execution host, or only the named one.
func showNodes(runner gedata.Runner, cfg *config.Config, name string, oneliner bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	nodes, err := gedata.Nodes(ctx, runner, cfg.GPU.GresComplex)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: %v\n", err)
		return 1
	}
	instances, err := gedata.QueueInstances(ctx, runner)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: %v\n", err)
		return 1
	}
	shown := 0
	for _, n := range nodes {
		if name != "" && n.Name != name {
			continue
		}
		printRecord(stdout, nodeRecord(n, hostInstances(instances, n.Name), cfg), oneliner)
		shown++
	}
	if name != "" && shown == 0 {
		fmt.Fprintf(stderr, "scontrol: error: Node %s not found\n", name)
		return 1
	}
	return 0
}

func hostInstances(instances []gedata.QueueInstance, host string) []gedata.QueueInstance {
	var out []gedata.QueueInstance
	for _, qi := range instances {
		if qi.Host == host {
			out = append(out, qi)
		}
	}
	return out
}

func nodeRecord(n gedata.Node, instances []gedata.QueueInstance, cfg *config.Config) record {
	alloc := 0
	for _, qi := range instances {
		alloc += qi.Used
	}
	gres := "(null)"
	if n.GPUs > 0 {
		gres = "gpu:" + strconv.Itoa(n.GPUs)
	}
	features := "(null)"
	if len(n.Features) > 0 {
		features = strings.Join(n.Features, ",")
	}
	return record{
		{{"NodeName", n.Name}, {"Arch", n.Arch}},
		{{"CPUAlloc", strconv.Itoa(alloc)}, {"CPUTot", strconv.Itoa(n.CPUs)}, {"CPULoad", fmt.Sprintf("%.2f", n.Load)}},
		{{"AvailableFeatures", features}},
		{{"ActiveFeatures", features}},
		{{"Gres", gres}},
		{{"RealMemory", strconv.FormatInt(n.MemoryMB, 10)}},
		{{"State", nodeState(n, instances, alloc)}},
		{{"Partitions", nodePartitions(instances, cfg)}},
	}
}

// nodeState folds the host's queue instances into one SLURM node state. A host
// whose execd delivers no load, or whose every instance is unreachable or in
// error, is DOWN; every instance disabled or suspended is DRAINED. Otherwise
// slot use decides: none IDLE, all MIXED or ALLOCATED.
func nodeState(n gedata.Node, instances []gedata.QueueInstance, alloc int) string {
	if !n.Reachable() || len(instances) == 0 {
		return "DOWN"
	}
	down, drained, total := 0, 0, 0
	for _, qi := range instances {
		total += qi.Total
		switch {
		case strings.ContainsAny(qi.States, "uEco"):
			down++
		case strings.ContainsAny(qi.States, "dDsSC"):
			drained++
		}
	}
	switch {
	case down == len(instances):
		return "DOWN"
	case down+drained == len(instances):
		return "DRAINED"
	case alloc == 0:
		return "IDLE"
	case alloc >= total || (n.CPUs > 0 && alloc >= n.CPUs):
		return "ALLOCATED"
	}
	return "MIXED"
}

// nodePartitions names the configured partitions whose queue has an instance
// on the host.
func nodePartitions(instances []gedata.QueueInstance, cfg *config.Config) string {
	var parts []string
	for name, p := range cfg.Partitions {
		for _, qi := range instances {
			if qi.Queue == p.Queue {
				parts = append(parts, name)
				break
			}
		}
	}
	if len(parts) == 0 {
		return "(null)"
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// showPartitions prints one record per configured partition (sorted), or only
// the named one. A partition is its queue's instances; MaxTime is the queue's
// h_rt.
func showPartitions(runner gedata.Runner, cfg *config.Config, name string, oneliner bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	var names []string
	for n := range cfg.Partitions {
		if name == "" || n == name {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		if name != "" {
			fmt.Fprintf(stderr, "scontrol: error: Partition %s not found\n", name)
		} else {
			fmt.Fprintln(stderr, "scontrol: error: no partitions configured")
		}
		return 1
	}
	sort.Strings(names)
	instances, err := gedata.QueueInstances(ctx, runner)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: %v\n", err)
		return 1
	}
	limits := map[string]string{} // one qconf -sq per queue, not per partition
	for _, n := range names {
		queue := cfg.Partitions[n].Queue
		if _, ok := limits[queue]; !ok {
			limit, err := gedata.QueueTimeLimit(ctx, runner, queue)
			if err != nil {
				fmt.Fprintf(stderr, "scontrol: error: %v\n", err)
				return 1
			}
			limits[queue] = limit
		}
		printRecord(stdout, partitionRecord(n, queue, cfg.DefaultPartition == n, limits[queue], instances), oneliner)
	}
	return 0
}

func partitionRecord(name, queue string, isDefault bool, hRT string, instances []gedata.QueueInstance) record {
	var hosts []string
	cpus, usable := 0, 0
	for _, qi := range instances {
		if qi.Queue != queue {
			continue
		}
		hosts = append(hosts, qi.Host)
		cpus += qi.Total
		if !strings.ContainsAny(qi.States, "uEcodDsSC") {
			usable++
		}
	}
	encoders.SortHosts(hosts)
	nodes := encoders.CompressNodelist(hosts)
	if nodes == "" {
		nodes = "(null)"
	}
	// UP while any instance takes jobs. With none, DOWN -- unless they are
	// only disabled (qmod -d), which keeps running jobs and refuses new ones:
	// SLURM's DRAIN.
	state := "UP"
	if usable == 0 {
		state = "DOWN"
		if len(hosts) > 0 && allDisabled(queue, instances) {
			state = "DRAIN"
		}
	}
	maxTime := "UNLIMITED"
	if secs, ok := geSeconds(hRT); ok {
		maxTime = slurmDuration(secs)
	}
	def := "NO"
	if isDefault {
		def = "YES"
	}
	return record{
		{{"PartitionName", name}},
		{{"Default", def}},
		{{"MaxTime", maxTime}},
		{{"Nodes", nodes}},
		{{"State", state}, {"TotalCPUs", strconv.Itoa(cpus)}, {"TotalNodes", strconv.Itoa(len(hosts))}},
	}
}

func allDisabled(queue string, instances []gedata.QueueInstance) bool {
	for _, qi := range instances {
		if qi.Queue == queue && !strings.ContainsAny(qi.States, "dD") {
			return false
		}
	}
	return true
}
