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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// srun used to resolve the per-job state dir to a shared /tmp/slurm_shim when
// TMPDIR was unset, reading layout.json and the step counter -- allocation truth
// -- from a world-known path any local user can plant files in (REQ-FAB-010).
var _ = Describe("srun per-job state resolution [REQ-FAB-010]", func() {
	It("resolves the state dir under a set TMPDIR", func() {
		GinkgoT().Setenv("TMPDIR", "/tmp/77.1.all.q")
		dir, err := stateDir()
		Expect(err).NotTo(HaveOccurred())
		Expect(dir).To(Equal("/tmp/77.1.all.q/slurm_shim"))
	})

	It("refuses an unset TMPDIR instead of falling back to a shared /tmp path", func() {
		GinkgoT().Setenv("TMPDIR", "")
		_, err := stateDir()
		Expect(err).To(MatchError(os.ErrNotExist))
	})

	It("reports the standalone case rather than reading /tmp for the layout", func() {
		GinkgoT().Setenv("TMPDIR", "")
		_, err := loadLayout()
		Expect(err).To(MatchError(errNoAllocation))
	})
})

var _ = Describe("srun outside an allocation", func() {
	BeforeEach(func() {
		GinkgoT().Setenv("JOB_ID", "")
		GinkgoT().Setenv("SLURM_JOB_ID", "")
	})

	It("may create a new allocation outside any job", func() {
		GinkgoT().Setenv("TMPDIR", "")
		_, err := loadLayout()
		Expect(allocationRefusal(err)).To(Succeed())
	})

	It("refuses a new allocation inside a Grid Engine job without a layout", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		GinkgoT().Setenv("JOB_ID", "4711")
		_, err := loadLayout()
		refusal := allocationRefusal(err)
		Expect(refusal).To(HaveOccurred())
		Expect(refusal.Error()).To(ContainSubstring("Grid Engine job 4711 is not a slurm-shim allocation"))
	})

	It("reports an unreadable layout instead of creating a new allocation", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		Expect(os.MkdirAll(filepath.Join(tmp, "slurm_shim", layout.LayoutFile), 0o700)).To(Succeed())
		_, err := loadLayout()
		refusal := allocationRefusal(err)
		Expect(refusal).To(HaveOccurred())
		Expect(refusal.Error()).To(ContainSubstring("reading layout"))
	})

	It("keeps the master-host message on a slave host", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		GinkgoT().Setenv("SLURM_JOB_ID", "4711")
		_, err := loadLayout()
		Expect(allocationRefusal(err)).To(MatchError(ContainSubstring("master host of job 4711")))
	})
})
