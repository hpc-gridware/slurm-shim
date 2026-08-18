package gedata_test

import (
	"context"
	"os"
	"path/filepath"
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

	It("falls back to $SGE_ROOT/bin/$ARC for a GE tool not on PATH", func() {
		// GE runs batch/PE scripts in non-login shells, so qstat & friends are
		// not on PATH; the runner must find them under $SGE_ROOT/bin/$ARC.
		root := GinkgoT().TempDir()
		arch := "test-arch"
		bin := filepath.Join(root, "bin", arch)
		Expect(os.MkdirAll(bin, 0o755)).To(Succeed())
		tool := filepath.Join(bin, "shim-fake-qstat")
		Expect(os.WriteFile(tool, []byte("#!/bin/sh\nprintf resolved\n"), 0o755)).To(Succeed())

		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("ARC", arch)

		out, _, exit, err := r.Run(ctx, "shim-fake-qstat")
		Expect(err).NotTo(HaveOccurred())
		Expect(exit).To(Equal(0))
		Expect(string(out)).To(Equal("resolved"))
	})
})

var _ = Describe("ResolveCommand", func() {
	// makeTool writes an executable file at path (parent dirs must exist).
	makeTool := func(path string) {
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte("#!/bin/sh\n:\n"), 0o755)).To(Succeed())
	}

	It("returns a name containing a path separator unchanged", func() {
		name := filepath.Join("sub", "tool")
		Expect(gedata.ResolveCommand(name)).To(Equal(name))
	})

	It("falls back to $SGE_ROOT/bin/$SGE_ARCH when ARC is empty", func() {
		root := GinkgoT().TempDir()
		cand := filepath.Join(root, "bin", "alt-arch", "shim-fake-qstat2")
		makeTool(cand)
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("ARC", "")
		GinkgoT().Setenv("SGE_ARCH", "alt-arch")
		Expect(gedata.ResolveCommand("shim-fake-qstat2")).To(Equal(cand))
	})

	It("prefers a name already on PATH over an $SGE_ROOT copy", func() {
		pathDir := GinkgoT().TempDir()
		makeTool(filepath.Join(pathDir, "shim-fake-pref"))
		root := GinkgoT().TempDir()
		makeTool(filepath.Join(root, "bin", "arch", "shim-fake-pref"))
		GinkgoT().Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("ARC", "arch")
		// On PATH: returned as the bare name so exec re-resolves it, not the SGE copy.
		Expect(gedata.ResolveCommand("shim-fake-pref")).To(Equal("shim-fake-pref"))
	})

	It("returns the bare name when nothing resolves", func() {
		root := GinkgoT().TempDir() // empty: no bin/arch/tool under it
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("ARC", "arch")
		Expect(gedata.ResolveCommand("shim-absent-tool")).To(Equal("shim-absent-tool"))
	})

	It("ignores a directory at the candidate path", func() {
		root := GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(root, "bin", "arch", "shim-dir-tool"), 0o755)).To(Succeed())
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("ARC", "arch")
		Expect(gedata.ResolveCommand("shim-dir-tool")).To(Equal("shim-dir-tool"))
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
