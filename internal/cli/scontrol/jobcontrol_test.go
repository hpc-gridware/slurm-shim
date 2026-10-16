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

package scontrol

import (
	"bytes"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("scontrol hold / uhold / release", func() {
	DescribeTable("runs one GE call per job, array indexes shifted to GE tasks",
		func(args []string, client string, targets []string) {
			r := &fake.Runner{}
			Expect(run(r, nil, args, io.Discard, io.Discard)).To(Equal(0))
			Expect(r.Calls).To(HaveLen(len(targets)))
			for i, c := range r.Calls {
				Expect(c.Name).To(Equal(client))
				Expect(c.Args).To(Equal([]string{targets[i]}))
			}
		},
		Entry("hold", []string{"hold", "4711"}, "qhold", []string{"4711"}),
		Entry("uhold is a user hold too", []string{"uhold", "4711"}, "qhold", []string{"4711"}),
		Entry("release", []string{"release", "4711"}, "qrls", []string{"4711"}),
		Entry("comma list with an array task", []string{"hold", "4711,4712_0"}, "qhold", []string{"4711", "4712.1"}),
		Entry("space list in one argument", []string{"release", "4711 4712_2"}, "qrls", []string{"4711", "4712.3"}),
		Entry("several arguments", []string{"hold", "1", "2,3"}, "qhold", []string{"1", "2", "3"}),
	)

	It("keeps going past a bad id and fails at the end, naming it", func() {
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if args[0] == "999999" {
				return fake.Response{Exit: 3, Stdout: []byte(`denied: job "999999" does not exist`)}
			}
			return fake.Response{Stdout: []byte("modified hold of job " + args[0])}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, nil, []string{"hold", "1,999999,abc,2"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(r.Calls).To(HaveLen(3), "abc never reaches GE")
		Expect(errBuf.String()).To(Equal("scontrol: error: 999999: denied: job \"999999\" does not exist\n" +
			"scontrol: error: Invalid job id specified: abc\n"))
	})

	It("needs a job id", func() {
		var errBuf bytes.Buffer
		Expect(run(&fake.Runner{}, nil, []string{"release"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no job id specified"))
	})
})

var _ = Describe("scontrol suspend / resume", func() {
	It("maps to qmod -sj and -usj", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("gridware - suspended job 4711")}
		}}
		Expect(run(r, nil, []string{"suspend", "4711_1"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Name).To(Equal("qmod"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-sj", "4711.2"}))

		r = &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("gridware - unsuspended job 4711")}
		}}
		Expect(run(r, nil, []string{"resume", "4711"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"-usj", "4711"}))
	})

	// qmod answers these with exit 0 (OCS 9.1.6); only its text tells.
	DescribeTable("reads qmod's answer, not its exit code",
		func(stdout string, exit int, wantCode int) {
			r := &fake.Runner{Responder: func(string, []string) fake.Response {
				return fake.Response{Stdout: []byte(stdout), Exit: exit}
			}}
			var errBuf bytes.Buffer
			Expect(run(r, nil, []string{"suspend", "1376"}, io.Discard, &errBuf)).To(Equal(wantCode))
			if wantCode != 0 {
				Expect(errBuf.String()).To(Equal("scontrol: error: 1376: " + stdout + "\n"))
			}
		},
		Entry("suspended", "gridware - suspended job 1376", 0, 0),
		Entry("already suspended", "gridware - job 1376 is already suspended", 0, 0),
		Entry("pending job refused with exit 0",
			"Modify operation can not be applied on job-array task 1376.1 in pending/hold state", 0, 1),
		Entry("unknown job", `invalid queue or job "1376"`, 1, 1),
	)
})
