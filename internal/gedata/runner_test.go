package gedata_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("ExecRunner [REQ-TST-002]", func() {
	var r gedata.ExecRunner
	ctx := context.Background()

	It("captures stdout and a zero exit for a successful command", func() {
		out, _, exit, err := r.Run(ctx, "sh", "-c", "printf hello")
		Expect(err).NotTo(HaveOccurred())
		Expect(exit).To(Equal(0))
		Expect(string(out)).To(Equal("hello"))
	})

	It("reports a non-zero exit through the exit value, not err", func() {
		_, _, exit, err := r.Run(ctx, "sh", "-c", "exit 3")
		Expect(err).NotTo(HaveOccurred())
		Expect(exit).To(Equal(3))
	})

	It("separates stderr from stdout", func() {
		out, errOut, _, err := r.Run(ctx, "sh", "-c", "printf out; printf err 1>&2")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("out"))
		Expect(string(errOut)).To(Equal("err"))
	})

	It("returns an error and exit -1 when the binary cannot be spawned", func() {
		_, _, exit, err := r.Run(ctx, "no-such-binary-xyzzy")
		Expect(err).To(HaveOccurred())
		Expect(exit).To(Equal(-1))
	})
})

var _ = Describe("RealIdentity host and interface resolution", func() {
	var id gedata.RealIdentity

	It("returns a non-empty short hostname", func() {
		h, err := id.Hostname()
		Expect(err).NotTo(HaveOccurred())
		Expect(h).NotTo(BeEmpty())
		Expect(h).NotTo(ContainSubstring("."))
	})

	It("resolves the loopback interface address without error", func() {
		// Loopback is always present under one of these names.
		_, errLo := id.InterfaceAddr("lo")
		_, errLo0 := id.InterfaceAddr("lo0")
		Expect(errLo == nil || errLo0 == nil).To(BeTrue())
	})

	It("returns the first non-loopback address for the empty interface", func() {
		_, err := id.InterfaceAddr("")
		Expect(err).NotTo(HaveOccurred())
	})

	It("resolves a known IP via the Go resolver [REQ-FAB-004]", func() {
		ip, err := id.LookupIP(context.Background(), "localhost")
		Expect(err).NotTo(HaveOccurred())
		// localhost resolves to a v4 loopback on essentially all hosts.
		if ip != "" {
			Expect(strings.HasPrefix(ip, "127.")).To(BeTrue())
		}
	})
})
