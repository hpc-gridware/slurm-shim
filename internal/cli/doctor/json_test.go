package doctor_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cli/doctor"
)

var _ = Describe("Run --json", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(fakeQconf), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte("#!/bin/sh\nexit 1\n"), 0o755)).To(Succeed())
		cfg := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n  batch: {queue: all.q, pe: make, slots: \"1\"}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		GinkgoT().Setenv("SGE_ROOT", "")
	})

	run := func(args ...string) (int, doctor.Document, string) {
		var stdout, stderr bytes.Buffer
		code := doctor.Run(args, &stdout, &stderr)
		var doc doctor.Document
		dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		dec.DisallowUnknownFields()
		Expect(dec.Decode(&doc)).To(Succeed(), stdout.String())
		Expect(dec.More()).To(BeFalse(), "stdout holds exactly one document")
		return code, doc, stdout.String()
	}

	// findings flattens a document into "level message" lines. A message that
	// spans lines (a qconf error carrying its stderr) shows only its first line
	// behind the prefix in the text, so only that is compared.
	findings := func(doc doctor.Document) []string {
		var out []string
		for _, s := range doc.Sections {
			for _, f := range s.Findings {
				out = append(out, f.Level+" "+strings.SplitN(f.Message, "\n", 2)[0])
			}
		}
		sort.Strings(out)
		return out
	}

	It("reports the same verdicts as the text, with the same exit code", func() {
		code, doc, _ := run("--json")
		Expect(doc.SchemaVersion).To(Equal(1))
		Expect(doc.Complete).To(BeTrue())

		var text, stderr bytes.Buffer
		Expect(doctor.Run(nil, &text, &stderr)).To(Equal(code))
		Expect(code).To(Equal(1))

		// Every PASS/WARN/FAIL/info line of the text is a finding, and nothing else.
		line := regexp.MustCompile(`^(PASS|WARN|FAIL|    )  (.*)$`)
		var fromText []string
		for _, l := range strings.Split(text.String(), "\n") {
			if m := line.FindStringSubmatch(l); m != nil {
				level := map[string]string{"PASS": "pass", "WARN": "warn", "FAIL": "fail", "    ": "info"}[m[1]]
				fromText = append(fromText, level+" "+m[2])
			}
		}
		sort.Strings(fromText)
		Expect(findings(doc)).To(Equal(fromText))
	})

	It("files the finding under its section and counts it in the summary", func() {
		_, doc, _ := run("--json")
		var network *doctor.Section
		fails, warns := 0, 0
		for i, s := range doc.Sections {
			if s.Name == "network" {
				network = &doc.Sections[i]
			}
			for _, f := range s.Findings {
				switch f.Level {
				case doctor.LevelFail:
					fails++
				case doctor.LevelWarn:
					warns++
				}
			}
		}
		Expect(network).NotTo(BeNil())
		Expect(network.Findings).To(ContainElement(SatisfyAll(
			HaveField("Level", doctor.LevelFail),
			HaveField("Message", ContainSubstring("qmaster_params sets ENABLE_RESCHEDULE_SLAVE")))))
		Expect(doc.Summary).To(Equal(doctor.Summary{Fail: fails, Warn: warns}))
	})

	It("names every section --offline did not run, and is still complete", func() {
		_, doc, out := run("--offline", "--json")
		Expect(doc.Offline).To(BeTrue())
		Expect(doc.Complete).To(BeTrue())
		Expect(doc.Skipped).To(HaveEach(HaveField("Reason", ContainSubstring("--offline"))))
		var names []string
		for _, s := range doc.Skipped {
			names = append(names, s.Name)
		}
		Expect(names).To(ConsistOf("wiring", "memory", "network", "security", "scheduler"))
		for _, s := range doc.Sections {
			Expect(names).NotTo(ContainElement(s.Name))
		}
		Expect(out).NotTo(ContainSubstring("paste this whole output"), "no banner on stdout")
	})

	It("marks a run that stopped at an unreadable config incomplete", func() {
		bad := filepath.Join(dir, "bad.yaml")
		Expect(os.WriteFile(bad, []byte("partitions: [\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", bad)

		code, doc, _ := run("--json")
		Expect(code).To(Equal(1))
		Expect(doc.Complete).To(BeFalse())
		Expect(doc.StoppedIn).To(Equal("config"))
		Expect(doc.Sections).To(ContainElement(SatisfyAll(
			HaveField("Name", "config"),
			HaveField("Findings", ContainElement(HaveField("Level", doctor.LevelFail))))))

		var text, stderr bytes.Buffer
		Expect(doctor.Run(nil, &text, &stderr)).To(Equal(1))
		Expect(text.String()).To(MatchRegexp(`\n\d+ FAIL, \d+ WARN\n$`), "the text report now ends with its summary too")
	})

	It("writes nothing to stdout on a usage error", func() {
		var stdout, stderr bytes.Buffer
		Expect(doctor.Run([]string{"--json", "--no-such-flag"}, &stdout, &stderr)).To(Equal(2))
		Expect(stdout.String()).To(BeEmpty())
	})
})
