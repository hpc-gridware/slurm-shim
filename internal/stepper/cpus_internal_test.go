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

package stepper

import (
	"bytes"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// The host half of the pinning rule (CS-2945): the stepper turns srun's planned
// positions into this host's CPUs, or leaves the ranks unpinned.
var _ = Describe("the CPUs a stepper gives its ranks", func() {
	// host stands in for a 14-CPU node whose job may use allowed.
	host := func(allowed ...int) func() (cpubind.Node, error) {
		return func() (cpubind.Node, error) {
			return cpubind.Node{Allowed: allowed, Online: 14}, nil
		}
	}
	all := func() []int {
		out := make([]int, 14)
		for i := range out {
			out[i] = i
		}
		return out
	}
	stepperFor := func(mode string, nodeSlots int, cpus func() (cpubind.Node, error), positions ...string) (*stepper, *bytes.Buffer) {
		spec := proto.StepSpec{CPUBind: mode, NodeSlots: nodeSlots}
		for i, p := range positions {
			spec.Ranks = append(spec.Ranks, proto.RankSpec{Rank: i, Local: i, Cpuset: p})
		}
		var stderr bytes.Buffer
		return &stepper{spec: spec, host: "node001", stderr: &stderr, hostCPUs: cpus}, &stderr
	}

	It("pins within the cores Grid Engine binding granted the job", func() {
		s, stderr := stepperFor(cpubind.ModeAuto, 2, host(6, 7), "0-0", "1-1")
		Expect(s.rankCPUs()).To(Equal([]string{"6", "7"}))
		Expect(stderr.String()).To(BeEmpty())
	})

	It("leaves the ranks unpinned on a node shared with other jobs", func() {
		s, stderr := stepperFor(cpubind.ModeAuto, 2, host(all()...), "0-0", "1-1")
		Expect(s.rankCPUs()).To(Equal([]string{"", ""}))
		Expect(stderr.String()).To(BeEmpty(), "unpinned on a shared node is the rule, not a fault")
	})

	It("pins from the first CPU on a node the job owns, as before", func() {
		s, _ := stepperFor(cpubind.ModeAuto, 14, host(all()...), "0-1", "2-3")
		Expect(s.rankCPUs()).To(Equal([]string{"0-1", "2-3"}))
	})

	It("pins on a node Grid Engine says the job holds whole, fewer slots than CPUs or not", func() {
		s, _ := stepperFor(cpubind.ModeAuto, 8, host(all()...), "0-0", "1-1")
		s.spec.HostSlots = 8
		Expect(s.rankCPUs()).To(Equal([]string{"0", "1"}))
	})

	It("leaves the ranks unpinned where Grid Engine says the host has more slots", func() {
		s, _ := stepperFor(cpubind.ModeAuto, 14, host(all()...), "0-0", "1-1")
		s.spec.HostSlots = 28
		Expect(s.rankCPUs()).To(Equal([]string{"", ""}), "a second queue on the host")
	})

	It("pins on a shared node when the step asked for binding", func() {
		s, _ := stepperFor(cpubind.ModeExplicit, 2, host(all()...), "0-0", "1-1")
		Expect(s.rankCPUs()).To(Equal([]string{"0", "1"}))
	})

	It("warns and pins nothing when the grant is too small for the ranks", func() {
		s, stderr := stepperFor(cpubind.ModeAuto, 2, host(5), "0-0", "1-1")
		Expect(s.rankCPUs()).To(Equal([]string{"", ""}))
		Expect(stderr.String()).To(ContainSubstring("stepper: warning: node001: the job may use 1 CPU(s) here (5), too few"))
	})

	It("warns and pins nothing when the host's CPUs cannot be read", func() {
		s, stderr := stepperFor(cpubind.ModeExplicit, 2, func() (cpubind.Node, error) {
			return cpubind.Node{}, errors.New("no sched_getaffinity")
		}, "0-0")
		Expect(s.rankCPUs()).To(Equal([]string{""}))
		Expect(stderr.String()).To(ContainSubstring("cannot read the CPUs this job may use on node001"))
	})

	It("still pins within a readable mask when the online count is unknown", func() {
		s, stderr := stepperFor(cpubind.ModeExplicit, 2, func() (cpubind.Node, error) {
			return cpubind.Node{Allowed: []int{3, 4}}, nil
		}, "0-0", "1-1")
		Expect(s.rankCPUs()).To(Equal([]string{"3", "4"}))
		Expect(stderr.String()).To(BeEmpty())
	})

	It("leaves a step planned without cpusets (--cpu-bind=none) unpinned anywhere", func() {
		s, _ := stepperFor(cpubind.ModeNone, 14, host(all()...), "", "")
		Expect(s.rankCPUs()).To(Equal([]string{"", ""}))
	})
})
