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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("Grid Engine value types", func() {
	DescribeTable("ParseTime",
		func(v string, secs int64, unlimited, ok bool) {
			gotSecs, gotUnlimited, gotOK := gedata.ParseTime(v)
			Expect([]any{gotSecs, gotUnlimited, gotOK}).To(Equal([]any{secs, unlimited, ok}))
		},
		Entry("seconds", "7200", int64(7200), false, true),
		Entry("h:m:s", "2:00:00", int64(7200), false, true),
		Entry("m:s", "1:30", int64(90), false, true),
		Entry("empty fields", "1::5", int64(3605), false, true),
		Entry("INFINITY", "infinity", int64(0), true, true),
		Entry("empty", "", int64(0), false, false),
		Entry("four fields", "1:2:3:4", int64(0), false, false),
		Entry("words", "soon", int64(0), false, false),
		Entry("negative", "-5", int64(0), false, false),
	)

	DescribeTable("MemBytes",
		func(v string, want float64, ok bool) {
			got, gotOK := gedata.MemBytes(v)
			Expect(gotOK).To(Equal(ok))
			Expect(got).To(Equal(want))
		},
		Entry("bytes", "1048576", float64(1<<20), true),
		Entry("K", "2K", float64(2048), true),
		Entry("G", "4G", float64(4<<30), true),
		Entry("T", "1T", float64(1<<40), true),
		Entry("decimal g", "1g", 1e9, true),
		Entry("a fraction", "1.5M", 1.5*(1<<20), true),
		Entry("empty", "", float64(0), false),
		Entry("words", "lots", float64(0), false),
	)
})
