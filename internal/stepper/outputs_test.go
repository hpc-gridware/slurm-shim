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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("rank output files", func() {
	var path string
	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "rank.out")
		Expect(os.WriteFile(path, []byte("before\n"), 0o644)).To(Succeed())
	})
	write := func(appendOutput bool, text string) {
		f, err := openOutput(path, appendOutput)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.WriteString(text)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())
	}

	It("truncates for a new step", func() {
		write(false, "after\n")
		Expect(os.ReadFile(path)).To(Equal([]byte("after\n")))
	})

	It("continues what the lost node wrote after a hot-spare relaunch", func() {
		write(true, "after\n")
		Expect(os.ReadFile(path)).To(Equal([]byte("before\nafter\n")))
	})

	It("never lets a stale writer overwrite the replacement's lines", func() {
		stale, err := openOutput(path, false) // the stepper on the lost host
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = stale.Close() }()
		write(true, "replacement\n")
		_, err = stale.WriteString("stale\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(os.ReadFile(path)).To(Equal([]byte("replacement\nstale\n")))
	})
})
