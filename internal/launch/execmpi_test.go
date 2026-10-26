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

package launch

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the MPI launcher probe", func() {
	script := func(body string) string {
		path := filepath.Join(GinkgoT().TempDir(), "mpirun")
		Expect(os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755)).To(Succeed())
		return path
	}

	It("returns the output of a launcher that prints its version and exits non-zero", func() {
		path, out, err := ProbeVersion(script("echo 'mpirun (Open MPI) 5.0.9'; exit 1\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(path).NotTo(BeEmpty())
		Expect(out).To(ContainSubstring("Open MPI"))
	})

	It("fails a launcher that exits non-zero without output", func() {
		_, _, err := ProbeVersion(script("exit 3\n"))
		Expect(err).To(MatchError(ContainSubstring("--version")))
	})

	It("tells a missing launcher from one it cannot run", func() {
		_, _, err := ProbeVersion("no-such-mpirun-here")
		Expect(errors.Is(err, ErrCommandNotFound)).To(BeTrue())
		_, _, err = ProbeVersion("/no/such/mpirun")
		Expect(errors.Is(err, ErrCommandNotFound)).To(BeTrue())
		plain := filepath.Join(GinkgoT().TempDir(), "plain")
		Expect(os.WriteFile(plain, []byte("x"), 0o644)).To(Succeed())
		_, _, err = ProbeVersion(plain)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, ErrCommandNotFound)).To(BeFalse())
	})

	It("does not wait for a wrapper's child that holds the output open", func() {
		saved := versionProbeTimeout
		versionProbeTimeout = 500 * time.Millisecond
		DeferCleanup(func() { versionProbeTimeout = saved })
		start := time.Now()
		_, _, err := ProbeVersion(script("sleep 30 &\nwait\n"))
		Expect(err).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", 10*time.Second))
	})
})
