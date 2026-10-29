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

package gedata

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	// go-clusterscheduler is the shared source of truth for OCS/GE command
	// formats; gedata is the shim's single boundary to it (callers use the
	// QueueInstance type below, never the library's). v9.1 is safe for both
	// supported releases: v9.1.FullQueueInfo is a type alias for the v9.0 struct
	// and the qstat -f columns it reads (queue@host, used/total, states) are the
	// stable classic ones, identical on OCS 9.0.10 through 9.1.5.
	qstat "github.com/hpc-gridware/go-clusterscheduler/pkg/qstat/v9.1"

	core "github.com/hpc-gridware/go-clusterscheduler/pkg/qconf/core"
)

// QueueInstance is one GE cluster-queue instance (queue@host) from qstat -f.
// States holds the raw GE state letters (e.g. "au", "d", "E"), empty when
// healthy.
type QueueInstance struct {
	Name   string // "all.q@ocs-master"
	Queue  string // "all.q"
	Host   string // "ocs-master"
	Used   int
	Total  int
	States string
}

// QueueInstances runs `qstat -f` through the runner and returns the parsed
// cluster-queue instances, of every host or only of hosts (`-q *@host,...`).
// Parsing is delegated to go-clusterscheduler; this function only adapts the
// result into the shim's QueueInstance type.
func QueueInstances(ctx context.Context, runner Runner, hosts ...string) ([]QueueInstance, error) {
	args := []string{"-f"}
	if len(hosts) > 0 {
		list := make([]string, len(hosts))
		for i, h := range hosts {
			list[i] = "*@" + h
		}
		args = append(args, "-q", strings.Join(list, ","))
	}
	out, errOut, exit, err := runner.Run(ctx, "qstat", args...)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat exited %d: %s", exit, strings.TrimSpace(string(errOut)))
	}
	full, err := qstat.ParseQstatFullOutput(string(out))
	if err != nil {
		return nil, err
	}
	instances := make([]QueueInstance, 0, len(full))
	for _, q := range full {
		queue, host, ok := strings.Cut(q.QueueName, "@")
		if !ok {
			continue // not a queue@host instance line
		}
		instances = append(instances, QueueInstance{
			Name:   q.QueueName,
			Queue:  queue,
			Host:   host,
			Used:   q.Used,
			Total:  q.Total,
			States: q.States,
		})
	}
	return instances, nil
}

// HostSlotCapacity returns, per host (short name), the slots it offers any
// job: the sum of its queue instances' slots, capped by its own
// complex_values slots limit. A job holding all of them has the host to
// itself. A host whose limit cannot be read counts its queue slots only --
// larger, so never mistaken for a host a job owns; one without queue
// instances is left out (unknown).
func HostSlotCapacity(ctx context.Context, r Runner, hosts []string) (map[string]int, error) {
	instances, err := QueueInstances(ctx, r, hosts...)
	if err != nil {
		return nil, err
	}
	capacity := map[string]int{}
	for _, qi := range instances {
		capacity[shortHost(qi.Host)] += qi.Total
	}
	for _, h := range hosts {
		short := shortHost(h)
		if _, ok := capacity[short]; !ok {
			continue
		}
		if limit, ok := hostSlotsLimit(ctx, r, h); ok && limit < capacity[short] {
			capacity[short] = limit
		}
	}
	return capacity, nil
}

// hostSlotsLimit reads a host's complex_values slots limit (qconf -se).
func hostSlotsLimit(ctx context.Context, r Runner, host string) (int, bool) {
	out, err := runChecked(ctx, r, "qconf", "-se", host)
	if err != nil {
		return 0, false
	}
	cfg, err := core.ParseExecHostConfigFromLines(strings.Split(string(out), "\n"))
	if err != nil {
		return 0, false
	}
	limit, err := strconv.Atoi(cfg.ComplexValues["slots"])
	if err != nil {
		return 0, false
	}
	return limit, true
}

func shortHost(h string) string {
	short, _, _ := strings.Cut(h, ".")
	return short
}
