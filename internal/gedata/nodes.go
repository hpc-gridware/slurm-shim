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
	"sort"
	"strings"

	qconf "github.com/hpc-gridware/go-clusterscheduler/pkg/qconf/core"
	qhost "github.com/hpc-gridware/go-clusterscheduler/pkg/qhost/v9.1"
)

// Node is one execution host as scontrol show node reports it. Slot usage and
// queue states are not here: they come from QueueInstances, which already reads
// `qstat -f` for sinfo.
type Node struct {
	Name     string
	Arch     string // "-" when the host reports no load values (execd down)
	CPUs     int
	MemoryMB int64
	Load     float64
	// Features are the BOOL complexes set true on the host (complex_values or
	// global), sorted: GE's equivalent of a SLURM node feature.
	Features []string
	// GPUs is the configured amount of the GPU complex on the host -- the
	// total, not what is free right now.
	GPUs int
}

// Reachable reports whether the host's execd delivers load values.
func (n Node) Reachable() bool { return n.Arch != "" && n.Arch != "-" }

// bytesPerMB converts qhost memory (bytes) to SLURM's RealMemory unit.
const bytesPerMB = 1024 * 1024

// Nodes returns every execution host. The host list, CPUs, memory and fixed
// complex values come from one `qhost -F`, parsed by go-clusterscheduler. The
// complex types (`qconf -sc`) decide which fixed values are features. qhost -F
// prints a consumable's FREE amount, so the configured GPU count is read from
// `qconf -se <host>`, once per host that has the complex at all.
func Nodes(ctx context.Context, r Runner, gpuComplex string) ([]Node, error) {
	out, err := runChecked(ctx, r, "qhost", "-F")
	if err != nil {
		return nil, err
	}
	hosts, err := qhost.ParseHostFullMetrics(string(out))
	if err != nil {
		return nil, fmt.Errorf("parse qhost -F: %w", err)
	}
	types, err := ComplexTypes(ctx, r)
	if err != nil {
		return nil, err
	}
	var nodes []Node
	for _, h := range hosts {
		if h.Name == "global" {
			continue // the pseudo host carrying cluster-wide values
		}
		n := Node{
			Name:     h.Name,
			Arch:     h.Arch,
			CPUs:     int(h.NumProc),
			MemoryMB: h.MemTotal / bytesPerMB,
			Load:     h.LoadAvg,
			Features: hostFeatures(h, types),
		}
		if _, ok := h.Resources[gpuComplex]; ok && gpuComplex != "" {
			if n.GPUs, err = configuredAmount(ctx, r, h.Name, gpuComplex); err != nil {
				return nil, err
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// hostFeatures lists the BOOL complexes with a fixed ("f") true value. A
// consumable BOOL (an EXCL complex such as exclusive) is a resource, not a
// label, and qhost marks it "c", so it stays out.
func hostFeatures(h qhost.HostFullMetrics, types map[string]string) []string {
	var out []string
	for name, res := range h.Resources {
		if types[name] != "BOOL" || res.Source != "F" {
			continue
		}
		if res.FloatValue == 1 || strings.EqualFold(res.StringValue, "true") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// configuredAmount reads the configured amount of complex on host from its
// complex_values (`qconf -se`). An RSMAP "2(0 1)" and a plain "2" both count 2.
func configuredAmount(ctx context.Context, r Runner, host, complex string) (int, error) {
	out, err := runChecked(ctx, r, "qconf", "-se", host)
	if err != nil {
		return 0, err
	}
	cfg, err := qconf.ParseExecHostConfigFromLines(strings.Split(string(out), "\n"))
	if err != nil {
		return 0, fmt.Errorf("parse qconf -se %s: %w", host, err)
	}
	value, ok := cfg.ComplexValues[complex]
	if !ok {
		return 0, nil // the value is global or queue-level, not the host's
	}
	amount, _, err := qconf.ParseResourceMap(value)
	if err != nil {
		return 0, fmt.Errorf("qconf -se %s: %w", host, err)
	}
	return amount, nil
}

// ComplexTypes maps every complex name to its type (BOOL, INT, RSMAP, ...)
// from the cluster's complex table.
func ComplexTypes(ctx context.Context, r Runner) (map[string]string, error) {
	cat, err := Complexes(ctx, r)
	if err != nil {
		return nil, err
	}
	types := make(map[string]string, len(cat))
	for _, c := range cat {
		types[c.Name] = c.Type
	}
	return types, nil
}

// QueueTimeLimit returns a cluster queue's default h_rt value from `qconf -sq`
// ("INFINITY", "48:00:00", "3600"); per-host overrides ("[host=...]") are left
// out. qconf prints one attribute per line when SGE_SINGLE_LINE is set, and a
// time limit never needs a continuation line; the library parser for this
// object runs its own qconf, so the one line is read here.
func QueueTimeLimit(ctx context.Context, r Runner, queue string) (string, error) {
	out, err := runChecked(ctx, r, "qconf", "-sq", queue)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "h_rt" {
			value, _, _ := strings.Cut(f[1], ",")
			return value, nil
		}
	}
	return "", fmt.Errorf("qconf -sq %s: no h_rt attribute", queue)
}

// runChecked runs a read-only GE client and turns a spawn failure or non-zero
// exit into an error naming the command and GE's own message.
func runChecked(ctx context.Context, r Runner, name string, args ...string) ([]byte, error) {
	out, errOut, exit, err := r.Run(ctx, name, args...)
	cmd := name + " " + strings.Join(args, " ")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("%s: exit %d: %s", cmd, exit, msg)
	}
	return out, nil
}
