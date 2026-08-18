package encoders_test

import (
	"fmt"
	"math/rand"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/encoders"
)

var _ = Describe("N1 nodelist compression", func() {
	DescribeTable("compresses host lists [REQ-ENC-001]",
		func(in []string, want string) {
			Expect(encoders.CompressNodelist(in)).To(Equal(want))
		},
		Entry("padded range with a gap", []string{"node001", "node002", "node003", "node007"}, "node[001-003,007]"),
		Entry("width change breaks range not group", []string{"n8", "n9", "n10"}, "n[8-9,10]"),
		Entry("unpadded range with a jump", []string{"node1", "node2", "node10"}, "node[1-2,10]"),
		Entry("distinct prefixes split blocks", []string{"gpu01", "gpu02", "cpu01"}, "gpu[01-02],cpu01"),
		Entry("prefix and suffix around digits", []string{"a1b", "a2b", "a3b"}, "a[1-3]b"),
		Entry("non-numeric names verbatim", []string{"alpha", "beta"}, "alpha,beta"),
		Entry("padded consecutive across width boundary", []string{"n09", "n10", "n11"}, "n[09-11]"),
		Entry("single host has no brackets", []string{"node042"}, "node042"),
		Entry("empty input", []string{}, ""),
	)

	It("preserves first-seen order and does not sort [REQ-ENC-002]", func() {
		Expect(encoders.CompressNodelist([]string{"node3", "node1", "node2"})).To(Equal("node[3,1-2]"))
	})
})

var _ = Describe("SortHosts (natural order for compression)", func() {
	It("orders unpadded names numerically, not lexically", func() {
		hosts := []string{"node10", "node2", "node1", "node11", "node3"}
		encoders.SortHosts(hosts)
		Expect(hosts).To(Equal([]string{"node1", "node2", "node3", "node10", "node11"}))
		// The whole point: natural order compresses into one contiguous range.
		Expect(encoders.CompressNodelist(hosts)).To(Equal("node[1-3,10-11]"))
	})

	It("groups by prefix then orders within a prefix", func() {
		hosts := []string{"ocs-worker2", "ocs-master", "ocs-worker1"}
		encoders.SortHosts(hosts)
		Expect(encoders.CompressNodelist(hosts)).To(Equal("ocs-master,ocs-worker[1-2]"))
	})

	It("falls back to lexical order for non-numeric names", func() {
		hosts := []string{"beta", "alpha"}
		encoders.SortHosts(hosts)
		Expect(hosts).To(Equal([]string{"alpha", "beta"}))
	})
})

var _ = Describe("N1' nodelist expansion", func() {
	DescribeTable("expands SLURM range syntax [REQ-ENC-003]",
		func(in string, want []string) {
			got, err := encoders.ExpandNodelist(in)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("padded range with gap", "node[001-003,007]", []string{"node001", "node002", "node003", "node007"}),
		Entry("width change term", "n[8-9,10]", []string{"n8", "n9", "n10"}),
		Entry("block plus verbatim", "gpu[01-02],cpu01", []string{"gpu01", "gpu02", "cpu01"}),
		Entry("suffix after bracket", "a[1-3]b", []string{"a1b", "a2b", "a3b"}),
		Entry("plain names", "alpha,beta", []string{"alpha", "beta"}),
		Entry("single host", "node042", []string{"node042"}),
	)

	DescribeTable("rejects malformed hostlists [REQ-ENC-003]",
		func(in string) {
			_, err := encoders.ExpandNodelist(in)
			Expect(err).To(HaveOccurred())
		},
		Entry("unmatched open", "node[1-2"),
		Entry("unmatched close", "node1-2]"),
		Entry("descending range", "node[5-2]"),
		Entry("non-numeric range", "node[a-c]"),
		Entry("empty", ""),
	)

	It("round-trips expand(compress(x)) == x for generated inputs [REQ-ENC-003]", func() {
		rng := rand.New(rand.NewSource(GinkgoRandomSeed()))
		for iter := 0; iter < 5000; iter++ {
			hosts := genHostList(rng)
			compressed := encoders.CompressNodelist(hosts)
			got, err := encoders.ExpandNodelist(compressed)
			Expect(err).NotTo(HaveOccurred(), "compress=%q", compressed)
			Expect(reflect.DeepEqual(got, hosts)).To(BeTrue(),
				"in=%v compress=%q out=%v", hosts, compressed, got)
		}
	})
})

// genHostList builds a random ordered hostname list from a small alphabet of
// prefixes, suffixes, and zero-padding widths, exercising the width-change and
// zero-padding edge cases the compressor must round-trip.
func genHostList(rng *rand.Rand) []string {
	prefixes := []string{"node", "n", "gpu", "cpu", "a", "host"}
	suffixes := []string{"", "", "", "b", ".local"}
	n := rng.Intn(6) + 1
	hosts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		switch rng.Intn(5) {
		case 0:
			// Non-numeric name.
			hosts = append(hosts, []string{"alpha", "beta", "gamma", "login"}[rng.Intn(4)])
		default:
			p := prefixes[rng.Intn(len(prefixes))]
			s := suffixes[rng.Intn(len(suffixes))]
			val := rng.Intn(200)
			width := rng.Intn(4) // 0 => natural width
			var digits string
			if width == 0 {
				digits = fmt.Sprintf("%d", val)
			} else {
				digits = fmt.Sprintf("%0*d", width, val)
			}
			hosts = append(hosts, p+digits+s)
		}
	}
	return hosts
}
