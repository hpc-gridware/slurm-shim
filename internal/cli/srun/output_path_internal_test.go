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

package srun

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

var _ = Describe("supervisor.outputPath [submitit Phase 2]", func() {
	// The array coordinates are resolved once onto the supervisor (from the
	// fabricated SLURM_ARRAY_* env); this exercises the env->pattern seam that
	// decides where submitit finds an array task's logs.
	sup := &supervisor{
		lay:         &layout.Layout{Job: layout.Job{JobID: 4711, Name: "myjob", User: "alice"}},
		stepID:      0,
		arrayJobID:  4720,
		arrayTaskID: 7, // SLURM 0-based
		user:        "alice",
	}
	r := plan.PlacedRank{Rank: 3, StepNodeIndex: 1}
	node := plan.StepNode{Host: "node002"}

	It("expands the array log path submitit reads (%A_%a_%t, 0-based)", func() {
		Expect(sup.outputPath("logs/%A_%a_%t_log.out", r, node)).To(Equal("logs/4720_7_3_log.out"))
	})

	It("expands the single-job verbs and job name / user", func() {
		Expect(sup.outputPath("%j_%t.out", r, node)).To(Equal("4711_3.out"))
		Expect(sup.outputPath("%x-%u-%N.log", r, node)).To(Equal("myjob-alice-node002.log"))
	})

	It("returns empty for an empty pattern (stream mode)", func() {
		Expect(sup.outputPath("", r, node)).To(Equal(""))
	})
})
