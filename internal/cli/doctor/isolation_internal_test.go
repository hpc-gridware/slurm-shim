package doctor

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("gpu.isolation cgroup verification", func() {
	devices := func(path string) map[string]string { return map[string]string{"devices": path} }

	Describe("devicesUndeclared", func() {
		It("names the instances with no devices characteristic", func() {
			ids := devicesUndeclared([]gedata.ResourceMapInstance{
				{ID: "gpu0", Characteristics: devices("/dev/nvidia0:rw;/dev/nvidiactl:r")},
				{ID: "gpu1"},
				{ID: "gpu2", Characteristics: map[string]string{"memory": "24G"}},
				{ID: "gpu3", Characteristics: devices(" ")},
			})
			Expect(ids).To(Equal([]string{"gpu1", "gpu2", "gpu3"}))
		})
	})

	Describe("isolationFindings", func() {
		It("passes only when every instance declares devices and systemd is on", func() {
			fails, warns, pass := isolationFindings("gpu", []hostIsolation{
				{Host: "n1", Defined: true, Instances: 2},
				{Host: "n2", Defined: true, Instances: 2},
				{Host: "login", Defined: false},
			})
			Expect(fails).To(BeEmpty())
			Expect(warns).To(BeEmpty())
			Expect(pass).To(ContainSubstring("all 4 RSMAP gpu instance(s) on 2 host(s)"))
		})

		It("FAILS a host whose instances declare no devices, because it fails open", func() {
			// The regression: doctor printed PASS "GE masks the devices" here
			// while a job on n2 could open every GPU on the host.
			fails, _, pass := isolationFindings("gpu", []hostIsolation{
				{Host: "n1", Defined: true, Instances: 2},
				{Host: "n2", Defined: true, Instances: 2, Undeclared: []string{"0", "1"}},
			})
			Expect(pass).To(BeEmpty())
			Expect(fails).To(HaveLen(1))
			Expect(fails[0]).To(ContainSubstring("n2"))
			Expect(fails[0]).To(ContainSubstring("0 1"))
			Expect(fails[0]).To(ContainSubstring("every GPU on the host"))
		})

		It("FAILS a host with systemd disabled, which confines nothing", func() {
			fails, _, pass := isolationFindings("gpu", []hostIsolation{
				{Host: "n1", Defined: true, Instances: 2, SystemdDisabled: true},
			})
			Expect(pass).To(BeEmpty())
			Expect(fails).To(ConsistOf(ContainSubstring("ENABLE_SYSTEMD=FALSE")))
		})

		It("warns rather than passes when no host defines the RSMAP", func() {
			fails, warns, pass := isolationFindings("gpu", []hostIsolation{{Host: "n1"}})
			Expect(fails).To(BeEmpty())
			Expect(pass).To(BeEmpty())
			Expect(warns).To(ConsistOf(ContainSubstring("no exec host defines RSMAP gpu")))
		})
	})
})
