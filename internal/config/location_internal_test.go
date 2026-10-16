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

package config

import (
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = DescribeTable("trustedOwner: who may own the config pointer and its directory",
	func(uid, euid, binUID int, binKnown, want bool) {
		gomega.Expect(trustedOwner(uid, euid, binUID, binKnown)).To(gomega.Equal(want))
	},
	Entry("root", 0, 1000, 500, true, true),
	Entry("the caller", 1000, 1000, 500, true, true),
	Entry("the owner of the install's binary (the OCS admin on a root-squashed share)", 500, 1000, 500, true, true),
	Entry("anyone else", 777, 1000, 500, true, false),
	Entry("anyone else, the binary's owner unknown", 500, 1000, -1, false, false),
)
