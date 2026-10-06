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
