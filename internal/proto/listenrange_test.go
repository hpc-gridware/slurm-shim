package proto_test

import (
	"net"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// portOf extracts the numeric port from a listener address.
func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	Expect(err).NotTo(HaveOccurred())
	n, err := strconv.Atoi(p)
	Expect(err).NotTo(HaveOccurred())
	return n
}

var _ = Describe("ListenRange [firewallable control port]", func() {
	const token = "0123456789abcdef0123456789abcdef"

	It("binds inside the requested range", func() {
		srv, err := proto.ListenRange("127.0.0.1", 63000, 200, token)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		Expect(portOf(srv.Addr())).To(And(
			BeNumerically(">=", 63000), BeNumerically("<", 63200)))
	})

	It("gives concurrent steps distinct ports (a single fixed port could not)", func() {
		var srvs []*proto.Server
		seen := map[int]bool{}
		for i := 0; i < 5; i++ {
			srv, err := proto.ListenRange("127.0.0.1", 63200, 200, token)
			Expect(err).NotTo(HaveOccurred())
			srvs = append(srvs, srv)
			p := portOf(srv.Addr())
			Expect(seen[p]).To(BeFalse(), "port %d handed out twice", p)
			seen[p] = true
		}
		for _, s := range srvs {
			Expect(s.Close()).To(Succeed())
		}
	})

	It("skips a port already taken rather than failing", func() {
		// Occupy the only port in a one-wide range, so the range is exhausted.
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = blocker.Close() }()
		taken := portOf(blocker.Addr().String())

		// A two-wide range containing the taken port must still succeed.
		srv, err := proto.ListenRange("127.0.0.1", taken, 2, token)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		Expect(portOf(srv.Addr())).To(Equal(taken + 1))
	})

	It("names the range and the knob when it is exhausted", func() {
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = blocker.Close() }()
		taken := portOf(blocker.Addr().String())

		_, err = proto.ListenRange("127.0.0.1", taken, 1, token)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no free control port"))
		Expect(err.Error()).To(ContainSubstring("control_port_range"))
		Expect(err.Error()).To(ContainSubstring(strconv.Itoa(taken)))
	})

	It("falls back to an ephemeral port when the range is disabled", func() {
		srv, err := proto.ListenRange("127.0.0.1", 0, 0, token)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		Expect(portOf(srv.Addr())).To(BeNumerically(">", 0))
	})
})

var _ = Describe("ListenRange error attribution", func() {
	const token = "0123456789abcdef0123456789abcdef"

	It("reports an unassignable bind host as itself, not as range exhaustion", func() {
		// 203.0.113.0/24 is TEST-NET-3: never assigned to a local interface, so the
		// bind fails identically on every port. Retrying the range would be pointless
		// and would blame the wrong setting.
		_, err := proto.ListenRange("203.0.113.199", 63000, 2000, token)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("binding control port"))
		Expect(err.Error()).NotTo(ContainSubstring("control_port_range"),
			"a bad bind host is not fixed by widening the range")
	})
})
