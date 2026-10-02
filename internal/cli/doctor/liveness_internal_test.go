package doctor

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("doctor: qmaster_params and execd_params switches", func() {
	DescribeTable("detects the qmaster_params switch that requeues a job on any slave loss",
		func(params string, want bool) {
			Expect(paramEnabled(params, "ENABLE_RESCHEDULE_SLAVE")).To(Equal(want))
		},
		Entry("unset", "NONE", false),
		Entry("bare", "ENABLE_RESCHEDULE_SLAVE", true),
		Entry("true", "ENABLE_RESCHEDULE_KILL=1,ENABLE_RESCHEDULE_SLAVE=true", true),
		Entry("1", "ENABLE_RESCHEDULE_SLAVE=1", true),
		Entry("explicitly off", "ENABLE_RESCHEDULE_SLAVE=false", false),
		Entry("another switch", "ENABLE_RESCHEDULE_KILL=1", false),
	)

	It("reads ENABLE_ADDGRP_KILL from execd_params", func() {
		Expect(paramEnabled("ENABLE_ADDGRP_KILL=TRUE", "ENABLE_ADDGRP_KILL")).To(BeTrue())
		Expect(paramEnabled("KEEP_ACTIVE=FALSE,enable_addgrp_kill=true", "ENABLE_ADDGRP_KILL")).To(BeTrue())
		Expect(paramEnabled("NONE", "ENABLE_ADDGRP_KILL")).To(BeFalse())
		Expect(paramEnabled("KEEP_ACTIVE=FALSE ENABLE_ADDGRP_KILL=TRUE", "ENABLE_ADDGRP_KILL")).To(BeTrue(),
			"a host configuration may separate entries by spaces")
	})
})

// confRunner answers qconf -sconf from a global configuration and per-host
// local ones; a host without an entry has no local configuration.
func confRunner(global string, local map[string]string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		if name != "qconf" || len(args) == 0 || args[0] != "-sconf" {
			return fake.Response{Exit: 1}
		}
		if len(args) == 1 {
			return fake.Response{Stdout: []byte(global)}
		}
		if conf, ok := local[args[1]]; ok {
			return fake.Response{Stdout: []byte("#" + args[1] + ":\n" + conf)}
		}
		return fake.Response{Stdout: []byte("configuration " + args[1] + " not defined\n"), Exit: 1}
	}}
}

var _ = Describe("doctor: ENABLE_ADDGRP_KILL per exec host", func() {
	ctx := context.Background()
	hosts := []string{"m", "w1", "w2"}

	It("passes a host-local ENABLE_ADDGRP_KILL=TRUE although the global one lacks it", func() {
		r := confRunner("execd_params                 NONE\n", map[string]string{
			"m":  "execd_params                 ENABLE_ADDGRP_KILL=TRUE\n",
			"w1": "execd_params                 KEEP_ACTIVE=FALSE,ENABLE_ADDGRP_KILL=TRUE\n",
			"w2": "execd_params                 ENABLE_ADDGRP_KILL=1\n",
		})
		Expect(addgrpKillMissing(ctx, r, hosts)).To(BeEmpty())
	})

	It("lets a host-local execd_params replace a global one that has the switch", func() {
		r := confRunner("execd_params                 ENABLE_ADDGRP_KILL=TRUE\n", map[string]string{
			"w1": "execd_params                 KEEP_ACTIVE=FALSE\n",
			"w2": "mailer                       /bin/mail\n",
		})
		Expect(addgrpKillMissing(ctx, r, hosts)).To(Equal([]string{"w1"}),
			"m has no local configuration and w2 none for execd_params: the global value applies to both")
	})

	It("names every host when neither config sets it", func() {
		r := confRunner("execd_params                 NONE\n", nil)
		Expect(addgrpKillMissing(ctx, r, hosts)).To(Equal(hosts))
	})

	It("is unknown, not missing, when qconf fails", func() {
		failing := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stderr: []byte("denied"), Exit: 1}
		}}
		missing, err := addgrpKillMissing(ctx, failing, hosts)
		Expect(err).To(MatchError(ContainSubstring("denied")))
		Expect(missing).To(BeNil())

		broken := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Err: errors.New("qconf: not found")}
		}}
		_, err = addgrpKillMissing(ctx, broken, hosts)
		Expect(err).To(HaveOccurred())
	})

	It("does not mistake an unreachable host for one without local configuration", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if len(args) == 1 {
				return fake.Response{Stdout: []byte("execd_params ENABLE_ADDGRP_KILL=TRUE\n")}
			}
			return fake.Response{Stderr: []byte(`can't resolve hostname "` + args[1] + `"`), Exit: 1}
		}}
		_, err := addgrpKillMissing(ctx, r, hosts)
		Expect(err).To(MatchError(ContainSubstring("resolve")))
	})
})

var _ = Describe("doctor: readConf", func() {
	It("reads multi-word values and skips the host comment line", func() {
		r := confRunner("", map[string]string{"w1": "execd_params A=1 B=2\n"})
		conf, found, err := readConf(context.Background(), r, "w1")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(conf).To(Equal(map[string]string{"execd_params": "A=1 B=2"}))
		Expect(strings.Join(r.Calls[0].Args, " ")).To(Equal("-sconf w1"))
	})
})

var _ = Describe("doctor: exec hosts that are not submit hosts", func() {
	It("names the hosts missing from the submit host list", func() {
		Expect(notIn([]string{"m", "w1", "w2"}, []string{"w1", "m"})).To(Equal([]string{"w2"}))
		Expect(notIn([]string{"m"}, []string{"m"})).To(BeEmpty())
	})
})

var _ = Describe("doctor: PE slots against the queues that offer it", func() {
	insts := []gedata.QueueInstance{
		{Queue: "all.q", Host: "h1", Total: 512}, {Queue: "all.q", Host: "h2", Total: 512},
		{Queue: "other.q", Host: "h1", Total: 64},
	}

	It("warns when the PE caps below the queue's slots, with the fix", func() {
		warn, _ := peSlotsFinding("make", 999, []string{"all.q"}, insts)
		Expect(warn).To(ContainSubstring("pe make caps every job using it, together, at 999 slots; queue(s) all.q have 1024"))
		Expect(warn).To(ContainSubstring("qconf -mattr pe slots 9999999 make"))
	})

	It("passes when the PE covers the queue", func() {
		warn, pass := peSlotsFinding("slurm-shim", 9999999, []string{"all.q"}, insts)
		Expect(warn).To(BeEmpty())
		Expect(pass).To(ContainSubstring("cover the 1024 slots"))
	})

	It("counts only the queues that offer the PE", func() {
		warn, _ := peSlotsFinding("smp", 1024, []string{"all.q"}, insts)
		Expect(warn).To(BeEmpty(), "other.q does not offer smp")
	})
})

var _ = Describe("doctor: PE slots with nothing to compare", func() {
	It("says nothing, rather than a vacuous PASS, when no queue offers the PE or its queues have no slots", func() {
		insts := []gedata.QueueInstance{{Queue: "all.q", Host: "h1", Total: 0}}
		for _, queues := range [][]string{nil, {"other.q"}, {"all.q"}} {
			warn, pass := peSlotsFinding("make", 999, queues, insts)
			Expect(warn + pass).To(BeEmpty())
		}
	})
})
