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

package sbatch

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// siteComplexes is a `qconf -sc` with the complexes a site adds for licenses,
// node features and whole nodes; mem_free is per host (consumable NO), as
// stock.
const siteComplexes = "#name shortcut type relop requestable consumable default urgency\n" +
	"mem_free            mf         MEMORY      <=    YES         NO         0        0\n" +
	"slots               s          INT         <=    YES         YES        1        1000\n" +
	"spectre_lic         spl        INT         <=    YES         JOB        0        0\n" +
	"flex_lic            flx        INT         <=    YES         NO         0        0\n" +
	"avx512              avx        BOOL        ==    YES         NO         0        0\n" +
	"exclusive           excl       BOOL        EXCL  YES         YES        0        1000\n"

// submitSite submits through run() against a cluster with those complexes and
// -par support, returning the qsub argv, stderr and the exit code.
func submitSite(complexes string, args ...string) ([]string, string, int) {
	var captured []string
	r := &fake.Runner{Responder: func(name string, a []string) fake.Response {
		switch {
		case name == "qconf":
			return fake.Response{Stdout: []byte(complexes)}
		case name == "qsub" && len(a) == 1 && a[0] == "-help":
			return fake.Response{Stdout: []byte(parUsage)}
		}
		captured = append([]string{}, a...)
		return fake.Response{Stdout: []byte("4711\n")}
	}}
	script := filepath.Join(GinkgoT().TempDir(), "job.sh")
	Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
	var out, errOut bytes.Buffer
	code := run(r, testCfg(), "/usr/bin/slurm-shim", append(args, script), &out, &errOut)
	return captured, errOut.String(), code
}

// hasRun reports whether want appears in argv as consecutive whole tokens:
// "-p -10" must not match "-p -100".
func hasRun(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// resourceList is the value of the -l that carries the job's resource requests
// (the last one; a partition's own request comes first).
func resourceList(argv []string) string {
	l := ""
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-l" {
			l = argv[i+1]
		}
	}
	return l
}

var _ = Describe("sbatch cluster resources [Phase 1b]", func() {
	DescribeTable("translates to the exact -l request",
		func(want string, args ...string) {
			argv, errOut, code := submitSite(siteComplexes, append([]string{"-p", "batch"}, args...)...)
			Expect(code).To(Equal(0), errOut)
			Expect(strings.Split(resourceList(argv), ",")).To(ConsistOf(strings.Split(want, ",")))
		},
		Entry("-L, a JOB-scope license", "spectre_lic=2", "-L", "spectre_lic:2"),
		Entry("--licenses, several", "spectre_lic=1,flex_lic=3", "--licenses=spectre_lic,flex_lic:3"),
		Entry("-C", "avx512=true", "-C", "avx512"),
		Entry("--constraint with &", "avx512=true", "--constraint=avx512&avx512"),
		Entry("--exclusive", "exclusive=true", "--exclusive"),
		Entry("-x", "h=!n3", "-x", "n3"),
		Entry("--exclude, a hostlist", "h=!(n3|n4)", "--exclude=n[3-4]"),
		Entry("-w", "h=n1|n2", "-w", "n[1-2]"),
		Entry("--mem-per-cpu on a per-host complex: x CPUs on the node",
			"mem_free=8G", "-N", "2", "--ntasks-per-node=2", "-c", "2", "--mem-per-cpu=2G"),
		Entry("--mem-per-gpu: x GPUs on the node", "mem_free=6G,gpu=2", "--gpus-per-node=2", "--mem-per-gpu=3G"),
		Entry("--mem stays per node", "mem_free=4G", "-N", "2", "-n", "8", "--mem=4G"),
	)

	It("asks one task's --mem-per-cpu per host when nothing pins the tasks per host", func() {
		argv, errOut, code := submitSite(siteComplexes, "-p", "batch", "-n", "64", "--mem-per-cpu=4G")
		Expect(code).To(Equal(0), errOut)
		Expect(strings.Split(resourceList(argv), ",")).To(ContainElement("mem_free=4G"), "not 64 x 4G on every host")
		Expect(errOut).To(ContainSubstring("requested for one task (1 CPU(s)) per host"))
	})

	It("runs -w on exactly the listed hosts: the node count follows the list", func() {
		argv, errOut, code := submitSite(siteComplexes, "-p", "batch", "-w", "n1,n2", "-n", "4")
		Expect(code).To(Equal(0), errOut)
		Expect(hasRun(argv, []string{"-pe", "smp.pe", "16"})).To(BeTrue(), "%v", argv)
	})

	It("keeps -w e away from a load-sensor license, which it cannot judge", func() {
		argv, errOut, code := submitSite(siteComplexes, "-p", "gpu", "-N", "2", "-L", "flex_lic:1")
		Expect(code).To(Equal(0), errOut)
		Expect(argv).To(ContainElement("-par"))
		Expect(argv).NotTo(ContainElement("-w"))
		Expect(errOut).To(ContainSubstring("checked, not reserved"))
	})

	It("keeps -w e away from --exclusive: it counts a running job's exclusive complex as taken", func() {
		argv, errOut, code := submitSite(siteComplexes, "-p", "gpu", "-N", "2", "--exclusive")
		Expect(code).To(Equal(0), errOut)
		Expect(argv).To(ContainElement("-par"))
		Expect(argv).NotTo(ContainElement("-w"))
	})

	DescribeTable("refuses what Grid Engine cannot express, submitting nothing",
		func(msg string, args ...string) {
			argv, errOut, code := submitSite(siteComplexes, append([]string{"-p", "batch"}, args...)...)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring("sbatch: error: "))
			Expect(errOut).To(ContainSubstring(msg))
			Expect(argv).To(BeEmpty(), "qsub never ran")
		},
		Entry("an unknown license", `unknown license "calibre"`, "-L", "calibre:1"),
		Entry("an either-or license list", "either-or", "-L", "spectre_lic|flex_lic"),
		Entry("an unknown feature", `unknown feature "h100"`, "-C", "h100"),
		Entry("either-or features", "only features joined by &", "-C", "avx512|h100"),
		Entry("fewer nodes than -w names", "fewer than the 2 host(s)", "-N", "1", "-w", "n1,n2"),
		Entry("more nodes than -w names: it would wait forever", "more than the 2 host(s)", "-N", "3", "-w", "n1,n2"),
		Entry("more nodes than -w leaves after -x", "more than the 1 host(s)", "-N", "2", "-w", "n1,n2", "-x", "n2"),
		Entry("-x removing every -w host", "leave no host", "-w", "n1", "-x", "n1"),
		Entry("a host list file", "from a file", "-w", "./hosts"),
		Entry("--mem-per-gpu without GPUs", "needs a GPU request", "--mem-per-gpu=2G"),
		Entry("an unreadable --begin", "--begin=whenever", "--begin=whenever"),
		Entry("--mem with an extra -l entry", "invalid memory value", "--mem=1G,slots=9"),
		Entry("--tmp with an extra -l entry", "invalid memory value", "--tmp=1G,h_rt=99999"),
		Entry("an -x host with an operator", "is not a host name", "-x", "a,!b"),
		Entry("a -w host with a request", "is not a host name", "-w", "n1,h_vmem=1"),
	)

	It("does not read the complex table for a job that names none of these", func() {
		var calls []string
		r := &fake.Runner{Responder: func(name string, a []string) fake.Response {
			calls = append(calls, name)
			return fake.Response{Stdout: []byte("4711\n")}
		}}
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
		Expect(run(r, testCfg(), "/usr/bin/slurm-shim", []string{"-p", "batch", script}, &bytes.Buffer{}, &bytes.Buffer{})).To(Equal(0))
		Expect(calls).NotTo(ContainElement("qconf"))
	})
})

var _ = Describe("sbatch job control [Phase 1c]", func() {
	DescribeTable("maps onto qsub flags",
		func(want []string, args ...string) {
			argv, errOut, code := submitSite(siteComplexes, append([]string{"-p", "batch"}, args...)...)
			Expect(code).To(Equal(0), errOut)
			Expect(hasRun(argv, want)).To(BeTrue(), "%v not in %v", want, argv)
		},
		Entry("-H", []string{"-h"}, "-H"),
		Entry("--hold", []string{"-h"}, "--hold"),
		Entry("--requeue", []string{"-r", "y"}, "--requeue"),
		Entry("--no-requeue", []string{"-r", "n"}, "--no-requeue"),
		Entry("--no-requeue wins over --signal's rerun", []string{"-notify", "-r", "n"}, "--signal=USR2@60", "--no-requeue"),
		Entry("--signal alone is rerunnable", []string{"-notify", "-r", "y"}, "--signal=USR2@60"),
		Entry("--mail-type=END,FAIL", []string{"-m", "ea"}, "--mail-type=END,FAIL"),
		Entry("--mail-type=ALL", []string{"-m", "bea"}, "--mail-type=ALL"),
		Entry("--mail-type=NONE", []string{"-m", "n"}, "--mail-type=NONE"),
		Entry("--mail-user", []string{"-M", "a@b.c"}, "--mail-user=a@b.c"),
		Entry("bare --nice lowers by 100", []string{"-p", "-100"}, "--nice"),
		Entry("--nice=10", []string{"-p", "-10"}, "--nice=10"),
		Entry("--nice clamps to Grid Engine's range", []string{"-p", "-1023"}, "--nice=5000"),
	)

	It("maps --begin to qsub -a", func() {
		argv, errOut, code := submitSite(siteComplexes, "-p", "batch", "--begin=2030-01-02T03:04:05")
		Expect(code).To(Equal(0), errOut)
		Expect(hasRun(argv, []string{"-a", "203001020304.05"})).To(BeTrue(), "%v", argv)
	})

	It("maps a relative --begin from the submit time", func() {
		argv, _, _ := submitSite(siteComplexes, "-p", "batch", "--begin=now+1hour")
		var stamp string
		for i, a := range argv {
			if a == "-a" {
				stamp = argv[i+1]
			}
		}
		t, err := time.ParseInLocation("200601021504.05", stamp, time.Local)
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Until(t)).To(BeNumerically("~", time.Hour, time.Minute))
	})

	It("names the mail types Grid Engine has no event for", func() {
		_, errOut, _ := submitSite(siteComplexes, "-p", "batch", "--mail-type=BEGIN,TIME_LIMIT_90")
		Expect(errOut).To(ContainSubstring("--mail-type TIME_LIMIT_90 ignored"))
	})
})

var _ = Describe("sbatch input environment [SBATCH_*]", func() {
	submitScript := func(body string, args ...string) []string {
		var captured []string
		r := &fake.Runner{Responder: func(name string, a []string) fake.Response {
			if name == "qsub" && (len(a) != 1 || a[0] != "-help") {
				captured = append([]string{}, a...)
			}
			return fake.Response{Stdout: []byte("4711\n")}
		}}
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte(body), 0o700)).To(Succeed())
		var errOut bytes.Buffer
		Expect(run(r, testCfg(), "/usr/bin/slurm-shim", append(args, script), &bytes.Buffer{}, &errOut)).To(Equal(0), errOut.String())
		return captured
	}
	name := func(argv []string) string {
		for i, a := range argv {
			if a == "-N" {
				return argv[i+1]
			}
		}
		return ""
	}

	It("overrides a script directive, and the command line overrides it", func() {
		GinkgoT().Setenv("SBATCH_JOB_NAME", "from-env")
		body := "#!/bin/bash\n#SBATCH -p batch\n#SBATCH -J from-script\n"
		Expect(name(submitScript(body))).To(Equal("from-env"))
		Expect(name(submitScript(body, "-J", "from-cli"))).To(Equal("from-cli"))
	})

	It("supplies the partition and the time limit", func() {
		GinkgoT().Setenv("SBATCH_PARTITION", "gpu")
		GinkgoT().Setenv("SBATCH_TIMELIMIT", "10")
		argv := submitScript("#!/bin/bash\n")
		Expect(argv).To(ContainElements("-q", "gpu.q"))
		Expect(resourceList(argv)).To(ContainSubstring("h_rt=600"))
	})

	It("sets a valueless flag by its presence", func() {
		GinkgoT().Setenv("SBATCH_REQUEUE", "")
		argv := submitScript("#!/bin/bash\n", "-p", "batch")
		Expect(strings.Join(argv, " ")).To(ContainSubstring("-r y"))
	})
})
