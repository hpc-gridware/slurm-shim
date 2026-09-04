package launch

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// The per-step token is staged by GE in the execd spool, so its confidentiality
// depends on that directory's reachability rather than on anything the shim
// controls. The preflight used to say so on every remote srun; these specs pin
// that it now only says so when a co-tenant could actually reach the spool.
var _ = Describe("token spool reachability [SI-51]", func() {
	conf := func(spool string) *fake.Runner {
		return &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte(
				"execd_spool_dir              " + spool + "\nqmaster_spool_dir            /x\n")}
		}}
	}

	It("stays silent when no other user can traverse to the spool", func() {
		base := GinkgoT().TempDir()
		closed := filepath.Join(base, "closed")
		Expect(os.Mkdir(closed, 0o700)).To(Succeed())
		Expect(os.Chmod(closed, 0o700)).To(Succeed())
		spool := filepath.Join(closed, "execd")
		Expect(os.Mkdir(spool, 0o755)).To(Succeed())

		Expect(tokenSpoolWarning(context.Background(), conf(spool))).To(BeEmpty())
	})

	It("names the spool and its mode when it is world-traversable", func() {
		// A real world-traversable path: a temp dir cannot be used, because its
		// ANCESTORS are private on macOS -- which is the function working, and
		// exactly the case the closed spec above covers.
		spool := os.TempDir()
		fi, err := os.Stat(spool)
		Expect(err).NotTo(HaveOccurred())
		if fi.Mode().Perm()&0o001 == 0 {
			Skip("this platform's temp dir is not world-traversable")
		}

		w := tokenSpoolWarning(context.Background(), conf(spool))
		Expect(w).To(ContainSubstring("traversable by other users"))
		Expect(w).To(ContainSubstring(spool), "the diagnostic must name the path to check")
		Expect(w).To(ContainSubstring("SI-51"))
	})

	It("keeps the original advisory when the spool dir cannot be determined", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1}
		}}
		Expect(tokenSpoolWarning(context.Background(), r)).To(
			ContainSubstring("confirm the execd env spool file is owner-only"))
	})

	It("keeps the original advisory when the spool path does not exist", func() {
		Expect(tokenSpoolWarning(context.Background(), conf("/nonexistent/execd/spool"))).To(
			ContainSubstring("confirm the execd env spool file is owner-only"))
	})
})
