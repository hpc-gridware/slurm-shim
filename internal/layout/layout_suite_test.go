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

package layout_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// stephelperPath is the gexec-built path to the step-counter helper, shared
// across parallel nodes for the multi-process concurrency spec (REQ-TST-005).
var stephelperPath string

var _ = SynchronizedBeforeSuite(func() []byte {
	p, err := gexec.Build("github.com/hpc-gridware/slurm-shim/internal/layout/stephelper")
	Expect(err).NotTo(HaveOccurred())
	return []byte(p)
}, func(data []byte) {
	stephelperPath = string(data)
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	gexec.CleanupBuildArtifacts()
})

func TestLayout(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Layout Suite")
}
