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

package fabricator

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SLURM_JOB_GPUS rendering [REQ-GPU-004]", func() {
	// SLURM documents SLURM_JOB_GPUS as the job's global GPU ids, and its own
	// env_uuid flag switches only CUDA_VISIBLE_DEVICES and ROCR_VISIBLE_DEVICES to
	// UUIDs, leaving this variable numeric. The shim makes the same split.
	It("passes numeric ids through unchanged", func() {
		Expect(jobGPUsValue([]string{"0", "1"})).To(Equal("0,1"))
	})

	It("preserves non-contiguous numeric ids rather than renumbering them", func() {
		// A grant of devices 4 and 5 must not be reported as 0,1: that would be a
		// different set of GPUs.
		Expect(jobGPUsValue([]string{"4", "5"})).To(Equal("4,5"))
	})

	It("falls back to grant positions when the ids are UUIDs", func() {
		// A UUID map has no numeric global id to report, so position is the only
		// honest answer.
		Expect(jobGPUsValue([]string{"GPU-0123456789abcdef", "GPU-dead0000000000ff"})).
			To(Equal("0,1"))
	})

	It("uses positions when only some ids are non-numeric", func() {
		Expect(jobGPUsValue([]string{"0", "GPU-dead0000000000ff"})).To(Equal("0,1"))
	})

	It("renders an empty grant as an empty value", func() {
		Expect(jobGPUsValue(nil)).To(Equal(""))
	})
})
