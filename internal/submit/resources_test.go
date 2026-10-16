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

package submit_test

import (
	"context"
	"time"
	_ "time/tzdata" // Europe/Berlin for the daylight-saving spec on any host

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// scTable is a `qconf -sc` with the complexes a site defines for licenses,
// features and whole nodes, next to stock ones.
const scTable = `#name               shortcut   type        relop requestable consumable default  urgency
#----------------------------------------------------------------------------------------
display_win_gui     dwg        BOOL        ==    YES         NO         0        0
exclusive           excl       BOOL        EXCL  YES         YES        0        1000
mem_free            mf         MEMORY      <=    YES         NO         0        0
rerun               re         BOOL        ==    NO          NO         0        0
slots               s          INT         <=    YES         YES        1        1000
spectre_lic         spl        INT         <=    YES         JOB        0        0
vcs_lic             vcs        INT         <=    YES         YES        0        0
flex_lic            flx        INT         <=    YES         NO         0        0
host_lic            hl         INT         <=    YES         HOST       0        0
avx512              avx        BOOL        ==    YES         NO         0        0
tmpdir              tmp        RESTRING    ==    NO          NO         0        0
`

func catalog() gedata.Catalog {
	r := &fake.Runner{Responder: func(string, []string) fake.Response {
		return fake.Response{Stdout: []byte(scTable)}
	}}
	cat, err := gedata.Complexes(context.Background(), r)
	Expect(err).NotTo(HaveOccurred())
	return cat
}

var _ = Describe("ResolveResources [licenses, features, exclusive, hosts, tmp]", func() {
	var cfg *config.Config
	BeforeEach(func() { cfg = config.Default() })

	resolve := func(r submit.Resources, slots int) (submit.Grant, error) {
		return submit.ResolveResources(cfg, catalog(), r, slots)
	}

	DescribeTable("translates exactly",
		func(r submit.Resources, slots int, want []string) {
			g, err := resolve(r, slots)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.Entries).To(Equal(want))
			if r.Exclusive || len(r.Nodelist) > 0 {
				Expect(g.Unverifiable).NotTo(BeEmpty(), "-w e counts a running job's exclusive complex as taken")
			} else {
				Expect(g.Unverifiable).To(BeEmpty())
			}
		},
		Entry("a JOB-scope license: the count once per job", submit.Resources{Licenses: "spectre_lic:2"}, 8, []string{"spectre_lic=2"}),
		Entry("no count means one", submit.Resources{Licenses: "spectre_lic"}, 4, []string{"spectre_lic=1"}),
		Entry("a per-slot license divides over the slots", submit.Resources{Licenses: "vcs_lic:8"}, 4, []string{"vcs_lic=2"}),
		Entry("by shortcut, emitted by name", submit.Resources{Licenses: "spl:3"}, 1, []string{"spectre_lic=3"}),
		Entry("several", submit.Resources{Licenses: "spectre_lic:1,vcs_lic:2"}, 2, []string{"spectre_lic=1", "vcs_lic=1"}),
		Entry("remote license name@server by its name", submit.Resources{Licenses: "spectre_lic@flexsrv:2"}, 1, []string{"spectre_lic=2"}),
		Entry("features joined by &", submit.Resources{Constraint: "avx512&display_win_gui"}, 1, []string{"avx512=true", "display_win_gui=true"}),
		Entry("a lone feature", submit.Resources{Constraint: "avx512"}, 1, []string{"avx512=true"}),
		Entry("--exclusive: the cluster's EXCL complex", submit.Resources{Exclusive: true}, 1, []string{"exclusive=true"}),
		Entry("-w: those hosts", submit.Resources{Nodelist: []string{"n1", "n2"}}, 2, []string{"h=n1|n2"}),
		Entry("-x one host", submit.Resources{Exclude: []string{"n3"}}, 1, []string{"h=!n3"}),
		Entry("-x several", submit.Resources{Exclude: []string{"n3", "n4"}}, 1, []string{"h=!(n3|n4)"}),
		Entry("-w minus -x", submit.Resources{Nodelist: []string{"n1", "n2"}, Exclude: []string{"n2"}}, 1, []string{"h=n1"}),
		Entry("nothing", submit.Resources{}, 1, []string(nil)),
	)

	DescribeTable("refuses what Grid Engine cannot express",
		func(r submit.Resources, slots int, msg string) {
			_, err := resolve(r, slots)
			Expect(err).To(MatchError(ContainSubstring(msg)))
		},
		Entry("an unknown license", submit.Resources{Licenses: "calibre:1"}, 1, `unknown license "calibre"`),
		Entry("an either-or license list", submit.Resources{Licenses: "spectre_lic:1|vcs_lic:1"}, 1, "either-or"),
		Entry("a per-slot license that does not divide", submit.Resources{Licenses: "vcs_lic:3"}, 2, "do not divide"),
		Entry("a per-host license", submit.Resources{Licenses: "host_lic:1"}, 1, "consumable JOB"),
		Entry("a license on a non-count complex", submit.Resources{Licenses: "avx512:1"}, 1, "not a count"),
		Entry("a zero count", submit.Resources{Licenses: "spectre_lic:0"}, 1, "invalid count"),
		Entry("an unknown feature", submit.Resources{Constraint: "gpu_h100"}, 1, `unknown feature "gpu_h100"`),
		Entry("a feature that is not BOOL", submit.Resources{Constraint: "slots"}, 1, "not BOOL"),
		Entry("an unrequestable complex", submit.Resources{Constraint: "rerun"}, 1, "not requestable"),
		Entry("either-or features", submit.Resources{Constraint: "avx512|display_win_gui"}, 1, "only features joined by &"),
		Entry("a feature count", submit.Resources{Constraint: "avx512*2"}, 1, "only features joined by &"),
		Entry("a bracketed set", submit.Resources{Constraint: "[avx512|display_win_gui]"}, 1, "only features joined by &"),
		Entry("a negation", submit.Resources{Constraint: "!avx512"}, 1, "only features joined by &"),
		Entry("-w entirely excluded", submit.Resources{Nodelist: []string{"n1"}, Exclude: []string{"n1"}}, 1, "no host"),
	)

	It("maps names through the config", func() {
		cfg.Licenses = map[string]string{"spectre": "spectre_lic"}
		cfg.Features = map[string]string{"intel": "avx512"}
		g, err := resolve(submit.Resources{Licenses: "spectre:2", Constraint: "intel"}, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(g.Entries).To(Equal([]string{"spectre_lic=2", "avx512=true"}))
	})

	It("requests a non-consumable license but says it is not reserved, and keeps -w e away", func() {
		g, err := resolve(submit.Resources{Licenses: "flex_lic:2"}, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(g.Entries).To(Equal([]string{"flex_lic=2"}))
		Expect(g.Unverifiable).To(ContainSubstring("load-sensor"))
		Expect(g.Warnings).To(ContainElement(ContainSubstring("checked, not reserved")))
	})

	It("uses the configured exclusive complex over auto-detection", func() {
		cfg.ExclusiveComplex = "my_excl"
		g, _ := resolve(submit.Resources{Exclusive: true}, 1)
		Expect(g.Entries).To(Equal([]string{"my_excl=true"}))
	})

	It("warns instead of guessing when the cluster has no exclusive complex", func() {
		g, err := submit.ResolveResources(cfg, gedata.Catalog{{Name: "slots", Type: "INT", Relop: "<="}},
			submit.Resources{Exclusive: true}, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(g.Entries).To(BeEmpty())
		Expect(g.Warnings).To(ContainElement(ContainSubstring("no exclusive complex")))
	})

	It("warns that --exclusive=user has no Grid Engine form", func() {
		g, _ := resolve(submit.Resources{ExclusiveMode: "user"}, 1)
		Expect(g.Entries).To(BeEmpty())
		Expect(g.Warnings).To(ContainElement(ContainSubstring("--exclusive=user ignored")))
	})

	It("maps --tmp to the configured complex, or warns", func() {
		g, _ := resolve(submit.Resources{Tmp: "10G"}, 1)
		Expect(g.Warnings).To(ContainElement(ContainSubstring("no tmp_complex")))
		cfg.TmpComplex = "local_disk"
		g, _ = resolve(submit.Resources{Tmp: "10G"}, 1)
		Expect(g.Entries).To(Equal([]string{"local_disk=10G"}))
	})
})

var _ = Describe("MemoryForScope [--mem-per-cpu, --mem-per-gpu]", func() {
	// 2 nodes x 4 slots; per-CPU: 4 CPUs per node, 8 in all.
	perCPU := submit.PerUnitMemory{Value: "1G", UnitsPerNode: 4, UnitsTotal: 8, SlotsPerNode: 4}
	DescribeTable("scales by the complex's consumable scope",
		func(m submit.PerUnitMemory, scope, want string) {
			got, err := submit.MemoryForScope(m, scope)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("per slot: one CPU is one slot", perCPU, "YES", "1G"),
		Entry("per host: CPUs on the node", perCPU, "NO", "4G"),
		Entry("per host (HOST)", perCPU, "HOST", "4G"),
		Entry("per job: every CPU", perCPU, "JOB", "8G"),
		Entry("unknown scope reads as per host", perCPU, "", "4G"),
		Entry("per GPU on a per-slot complex spreads over the node's slots",
			submit.PerUnitMemory{Value: "3G", UnitsPerNode: 2, UnitsTotal: 4, SlotsPerNode: 4}, "YES", "1536M"),
		Entry("MB in, rounded up", submit.PerUnitMemory{Value: "100M", UnitsPerNode: 3, UnitsTotal: 3, SlotsPerNode: 3}, "NO", "300M"),
		Entry("fractional", submit.PerUnitMemory{Value: "1.5G", UnitsPerNode: 2, UnitsTotal: 2, SlotsPerNode: 2}, "NO", "3G"),
	)
	It("rejects a value it cannot read", func() {
		_, err := submit.MemoryForScope(submit.PerUnitMemory{Value: "lots", UnitsPerNode: 1}, "NO")
		Expect(err).To(MatchError(ContainSubstring("invalid memory value")))
	})
})

var _ = Describe("ParseBegin [--begin]", func() {
	loc := time.FixedZone("test", 2*3600)
	now := time.Date(2026, 10, 12, 14, 30, 0, 0, loc)
	at := func(mo time.Month, d, h, mi, s int) time.Time { return time.Date(2026, mo, d, h, mi, s, 0, loc) }

	DescribeTable("reads SLURM's forms",
		func(v string, want time.Time) {
			got, err := submit.ParseBegin(v, now)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("now", "now", now),
		Entry("now+60 is seconds", "now+60", now.Add(time.Minute)),
		Entry("now+1hour", "now+1hour", now.Add(time.Hour)),
		Entry("now+2days", "now+2days", now.AddDate(0, 0, 2)),
		Entry("a later time today", "16:00", at(10, 12, 16, 0, 0)),
		Entry("a passed time is tomorrow", "09:15:30", at(10, 13, 9, 15, 30)),
		Entry("PM", "4:00 PM", at(10, 12, 16, 0, 0)),
		Entry("12 AM is midnight", "12:00am", at(10, 13, 0, 0, 0)),
		Entry("midnight", "midnight", at(10, 13, 0, 0, 0)),
		Entry("noon has passed", "noon", at(10, 13, 12, 0, 0)),
		Entry("teatime is still ahead", "teatime", at(10, 12, 16, 0, 0)),
		Entry("today", "today", at(10, 12, 0, 0, 0)),
		Entry("tomorrow", "tomorrow", at(10, 13, 0, 0, 0)),
		Entry("MMDD", "1224", at(12, 24, 0, 0, 0)),
		Entry("MMDDYY", "122427", time.Date(2027, 12, 24, 0, 0, 0, 0, loc)),
		Entry("MM/DD", "12/24", at(12, 24, 0, 0, 0)),
		Entry("MM.DD.YY", "12.24.27", time.Date(2027, 12, 24, 0, 0, 0, 0, loc)),
		Entry("date and time", "12/24-18:30", at(12, 24, 18, 30, 0)),
		Entry("ISO date", "2026-11-01", at(11, 1, 0, 0, 0)),
		Entry("ISO date and time", "2026-11-01T08:05:09", at(11, 1, 8, 5, 9)),
		Entry("a passed date without a year is next year's", "0105", time.Date(2027, 1, 5, 0, 0, 0, 0, loc)),
		Entry("today's date without a year stays today", "10/12", at(10, 12, 0, 0, 0)),
		Entry("a passed date with a year stays in the past", "2026-01-05", at(1, 5, 0, 0, 0)),
	)
	DescribeTable("refuses what it cannot read",
		func(v string) {
			_, err := submit.ParseBegin(v, now)
			Expect(err).To(MatchError(ContainSubstring("--begin=" + v)))
		},
		Entry("words", "next tuesday"),
		Entry("a bad hour", "25:00"),
		Entry("a bad unit", "now+3fortnights"),
		Entry("a bad month", "2026-13-01"),
		Entry("a day the month does not have", "02/30"),
		Entry("an ISO day the month does not have", "2026-04-31"),
		Entry("hours that overflow a duration", "now+9999999999hours"),
		Entry("weeks beyond a century", "now+9999999w"),
	)
	It("keeps wall-clock times on a daylight-saving change day", func() {
		berlin, err := time.LoadLocation("Europe/Berlin")
		Expect(err).NotTo(HaveOccurred())
		// 2026-03-29 has 23 hours in Berlin: clocks jump from 02:00 to 03:00.
		dstNow := time.Date(2026, 3, 29, 1, 0, 0, 0, berlin)
		for v, want := range map[string]time.Time{
			"16:00":       time.Date(2026, 3, 29, 16, 0, 0, 0, berlin),
			"noon":        time.Date(2026, 3, 29, 12, 0, 0, 0, berlin),
			"03/29-18:30": time.Date(2026, 3, 29, 18, 30, 0, 0, berlin),
		} {
			got, err := submit.ParseBegin(v, dstNow)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Equal(want)).To(BeTrue(), "%s: got %s, want %s", v, got, want)
		}
	})
	It("formats the qsub -a stamp", func() {
		Expect(submit.BeginStamp(at(11, 1, 8, 5, 9))).To(Equal("202611010805.09"))
	})
})
