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

package installcmd

import (
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/install"
)

func TestInstallCmd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "InstallCmd Suite")
}

var _ = Describe("printPlan", func() {
	It("renders an addition to pe_list as +=, not as a replacement", func() {
		var b bytes.Buffer
		printPlan(&b, install.Plan{Changes: []install.Change{
			{Kind: install.ChangeAddToPEList, Object: "all.q", Attr: "pe_list", Old: "make smp", New: "slurm-shim"},
		}})
		Expect(b.String()).To(ContainSubstring("make smp += slurm-shim"))
		Expect(b.String()).NotTo(ContainSubstring("->"))
	})

	It("renders a refusal with its reason and an unset old value as NONE", func() {
		var b bytes.Buffer
		printPlan(&b, install.Plan{Changes: []install.Change{
			{Kind: install.ChangeRefused, Object: "all.q", Attr: "starter_method", Old: "/site/s.sh", New: "/x", Reason: "two starters cannot be chained"},
			{Kind: install.ChangeSetStarter, Object: "gpu.q", Attr: "starter_method", Old: "", New: "/x"},
		}})
		Expect(b.String()).To(ContainSubstring("REFUSED"))
		Expect(b.String()).To(ContainSubstring("two starters cannot be chained"))
		Expect(b.String()).To(ContainSubstring("NONE -> /x"))
	})
})

var _ = Describe("writeConfigAtomic", func() {
	var dir string
	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("creates a new config readable by the users who run jobs", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(writeConfigAtomic(path, []byte("a: 1\n"))).To(Succeed())
		fi, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("PRESERVES a restricted mode instead of widening it on upgrade", func() {
		// A site that chose 0600 must not have it silently reopened to the world
		// by an install. os.WriteFile's mode argument applies only on create, so
		// the old code happened to preserve this -- writing via a temp file does
		// not, unless it is carried over deliberately.
		path := filepath.Join(dir, "config.yaml")
		Expect(os.WriteFile(path, []byte("a: 1\n"), 0o600)).To(Succeed())
		Expect(writeConfigAtomic(path, []byte("a: 2\n"))).To(Succeed())
		fi, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("leaves no temp files behind", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(writeConfigAtomic(path, []byte("a: 1\n"))).To(Succeed())
		Expect(writeConfigAtomic(path, []byte("a: 2\n"))).To(Succeed())
		ents, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(ents).To(HaveLen(1))
		Expect(ents[0].Name()).To(Equal("config.yaml"))
	})

	It("writes the content it was given", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(writeConfigAtomic(path, []byte("kill_wait: 45s\n"))).To(Succeed())
		Expect(os.ReadFile(path)).To(Equal([]byte("kill_wait: 45s\n")))
	})
})

var _ = Describe("placeTree [starter trust boundary]", func() {
	var src, prefix, me string
	var stdout, stderr bytes.Buffer

	BeforeEach(func() {
		u, err := user.Current()
		Expect(err).NotTo(HaveOccurred())
		me = u.Username
		src = GinkgoT().TempDir()
		for rel, body := range map[string]string{
			install.BinaryRel:  "binary",
			install.StarterRel: "#!/bin/sh\n",
			install.HookRel:    "# hook\n",
		} {
			Expect(os.MkdirAll(filepath.Join(src, filepath.Dir(rel)), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(src, rel), []byte(body), 0o755)).To(Succeed())
		}
		prefix = filepath.Join(GinkgoT().TempDir(), "slurm-shim")
		stdout.Reset()
		stderr.Reset()
	})

	It("goes on when the tree is owned by an allowed principal", func() {
		Expect(placeTree(src, prefix, me, &stdout, &stderr)).To(BeTrue())
		Expect(stderr.String()).To(BeEmpty())
		Expect(stdout.String()).To(ContainSubstring("installed  " + prefix))
	})

	It("stops when the tree is owned by someone who is not root or the admin user", func() {
		// The tarball case: unpacked as root, the files keep the build user's
		// uid. Here the test user plays that stranger, with only root allowed.
		if os.Geteuid() == 0 {
			Skip("as root, the tree is root-owned and passes")
		}
		Expect(placeTree(src, prefix, "", &stdout, &stderr)).To(BeFalse())
		out := stderr.String()
		Expect(out).To(ContainSubstring(prefix + " must not be a starter_method"))
		Expect(out).To(ContainSubstring(filepath.Join(prefix, install.StarterRel)))
		Expect(out).To(ContainSubstring("must be owned by root and not group- or world-writable"))
		Expect(out).To(ContainSubstring("chown -R root:root " + prefix + " && chmod -R go-w " + prefix))
		Expect(out).To(ContainSubstring("The cluster was not changed"))
	})

	It("stops on a group-writable directory above the tree", func() {
		// Anyone in that group can rename the tree and put their own starter in
		// its place, so this is as unsafe as a writable starter.
		parent := filepath.Dir(prefix)
		Expect(os.Chmod(parent, 0o775)).To(Succeed())
		Expect(placeTree(src, prefix, me, &stdout, &stderr)).To(BeFalse())
		Expect(stderr.String()).To(ContainSubstring(parent + ": group- or world-writable"))
		Expect(stderr.String()).To(ContainSubstring("owned by root or " + me))
	})
})
