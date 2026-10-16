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

package gedata_test

import (
	"context"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// ocs9010 answers like the live OCS 9.0.10 capture: job 1 is held by its user
// (sbatch -H), and no job has an operator or system hold.
func ocs9010(name string, args []string) fake.Response {
	file := ""
	switch strings.Join(append([]string{name}, args...), " ") {
	case "qstat -xml -u * -s hu":
		file = "qstat_s_hu_9.0.10.xml"
	case "qstat -xml -u * -s ho", "qstat -xml -u * -s hs":
		file = "qstat_s_ho_empty_9.0.10.xml"
	case "qstat -xml -j 1":
		file = "qstat_j_held_9.0.10.xml"
	default:
		return fake.Response{Exit: 1, Stderr: []byte("unexpected call")}
	}
	data, err := os.ReadFile("testdata/" + file)
	Expect(err).NotTo(HaveOccurred())
	return fake.Response{Stdout: data}
}

var _ = Describe("hold kinds on OCS 9.0 [gedata.HoldKinds]", func() {
	It("finds no hold in a 9.0.10 qstat -xml -j: the hold ranges are left out", func() {
		set, err := gedata.JobInfos(context.Background(), &fake.Runner{Responder: ocs9010}, []string{"1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.Jobs).To(HaveKey("1"))
		Expect(set.Jobs["1"].Hold("")).To(Equal(gedata.HoldNone))
	})

	It("reads the kind from qstat -s hu|ho|hs instead", func() {
		r := &fake.Runner{Responder: ocs9010}
		kinds, err := gedata.HoldKinds(context.Background(), r)
		Expect(err).NotTo(HaveOccurred())
		Expect(kinds).To(Equal(map[string]string{"1": gedata.HoldUser}))
		Expect(r.Calls).To(HaveLen(3))
	})

	It("names an operator hold admin, and a user hold wins over it", func() {
		hu, err := os.ReadFile("testdata/qstat_s_hu_9.0.10.xml")
		Expect(err).NotTo(HaveOccurred())
		empty, err := os.ReadFile("testdata/qstat_s_ho_empty_9.0.10.xml")
		Expect(err).NotTo(HaveOccurred())
		answers := map[string][]byte{"hu": empty, "ho": hu, "hs": hu}
		kinds, err := gedata.HoldKinds(context.Background(), &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			return fake.Response{Stdout: answers[args[len(args)-1]]}
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(kinds).To(Equal(map[string]string{"1": gedata.HoldAdmin}))

		answers["hu"] = hu
		kinds, err = gedata.HoldKinds(context.Background(), &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			return fake.Response{Stdout: answers[args[len(args)-1]]}
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(kinds).To(Equal(map[string]string{"1": gedata.HoldUser}))
	})

	It("reports a failing qstat", func() {
		_, err := gedata.HoldKinds(context.Background(), &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("denied")}
		}})
		Expect(err).To(MatchError(ContainSubstring("qstat -s hu: exit 1: denied")))
	})
})
