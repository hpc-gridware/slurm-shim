package srun_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// fakeQrsh stands in for `qrsh -inherit` on one machine: it takes the -v
// variables into the environment, drops the target host, and runs the stepper
// here, as the target's execd would. The real QrshLauncher drives it, so every
// start waits the launcher's rejection window, as on a cluster.
const fakeQrsh = `#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in
    -inherit|-nostdin|-noshell) shift ;;
    -v) export "$2"; shift 2 ;;
    *) break ;;
  esac
done
shift
exec "$@"
`

// qrshAlloc writes an n-node layout (one task per node) whose remote nodes are
// launched through the qrsh launcher, and a PATH with the fake qrsh.
func qrshAlloc(n int) (tmp string, env []string) {
	nodes := make([]layout.Node, n)
	perNode := make([]int, n)
	for i := range nodes {
		nodes[i] = layout.Node{Index: i, Host: fmt.Sprintf("node%03d", i+1), Slots: 1, IsMaster: i == 0}
		perNode[i] = 1
	}
	tmp = writeAlloc(nodes, perNode)
	Expect(layout.Update(filepath.Join(tmp, layout.StateDir), func(l *layout.Layout) error {
		l.Rendezvous.MasterAddr = "127.0.0.1" // where the steppers dial srun
		return nil
	})).To(Succeed())
	Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
		[]byte("launcher: qrsh-inherit\ncontrol_port_base: 0\n"), 0o600)).To(Succeed())
	bin := filepath.Join(tmp, "bin")
	Expect(os.MkdirAll(bin, 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(bin, "qrsh"), []byte(fakeQrsh), 0o755)).To(Succeed())
	return tmp, []string{"PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin"}
}

var _ = Describe("srun launch at scale", func() {
	// A smoke test through the real qrsh launcher: every start waits qrsh's 2s
	// rejection window, and a connected stepper is held only briefly until srun
	// accepts it. One at a time this step needed over 2 minutes and failed;
	// that the launch is concurrent is proven deterministically in
	// launch_internal_test.go, so the bound here is generous.
	It("launches a 64-node step through qrsh", func() {
		tmp, env := qrshAlloc(64)
		start := time.Now()
		sess := runSrunEnv(tmp, env, "-N", "64", "sh", "-c", `echo "rank $SLURM_PROCID up"`)
		Eventually(sess, "120s").Should(gexec.Exit())
		Expect(sess.ExitCode()).To(Equal(0), string(sess.Err.Contents()))
		Expect(strings.Count(string(sess.Out.Contents()), " up")).To(Equal(64))
		GinkgoWriter.Printf("64-node step took %s\n", time.Since(start))
		Expect(time.Since(start)).To(BeNumerically("<", 60*time.Second), "one at a time it would take over 2 minutes")
	})
})
