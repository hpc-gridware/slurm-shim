package launch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/launch"
)

var _ = Describe("RunSession [srun outside an allocation, no terminal]", func() {
	var dir string
	// fakeQrsh puts a qrsh on PATH that records its argv and then runs body.
	fakeQrsh := func(body string) {
		dir = GinkgoT().TempDir()
		script := "#!/bin/sh\necho \"$@\" > " + dir + "/argv\n" + body + "\n"
		Expect(os.WriteFile(filepath.Join(dir, "qrsh"), []byte(script), 0o755)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SGE_ROOT", "")
	}
	spec := launch.SessionSpec{Queue: "all.q", PE: "make", Slots: 1, Command: []string{"/bin/true"}}

	It("returns the command's exit status", func() {
		fakeQrsh("exit 7")
		code, err := launch.RunSession(spec, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(7))
		Expect(os.ReadFile(filepath.Join(dir, "argv"))).To(ContainSubstring("-now no -cwd -q all.q -pe make 1 /bin/true"))
	})

	It("passes SIGTERM on as SIGINT, the one signal qrsh acts on, and exits 128+15", func() {
		dir := GinkgoT().TempDir()
		script := filepath.Join(dir, "qrsh")
		Expect(os.WriteFile(script, []byte("#!/bin/sh\n"+
			"trap 'echo INT > "+dir+"/got; exit 1' INT\n"+
			"trap 'echo TERM > "+dir+"/got; exit 1' TERM\n"+
			"touch "+dir+"/started\n"+
			"i=0; while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done\n"), 0o755)).To(Succeed())
		sigs := make(chan os.Signal, 1)
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := launch.Supervise(exec.Command(script), sigs)
			done <- result{code, err}
		}()
		Eventually(filepath.Join(dir, "started"), 5*time.Second).Should(BeAnExistingFile())
		sigs <- syscall.SIGTERM
		var r result
		Eventually(done, 10*time.Second).Should(Receive(&r))
		Expect(r.err).NotTo(HaveOccurred())
		Expect(r.code).To(Equal(128 + 15))
		Expect(os.ReadFile(filepath.Join(dir, "got"))).To(ContainSubstring("INT"))
	})

	It("reports a command killed by a signal as 128+signal", func() {
		dir := GinkgoT().TempDir()
		script := filepath.Join(dir, "qrsh")
		Expect(os.WriteFile(script, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755)).To(Succeed())
		code, err := launch.Supervise(exec.Command(script), make(chan os.Signal))
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(128 + 9))
	})

	// A running qrsh ends the job on SIGINT yet exits 0 (OCS 9.1.6): the
	// status must still say the step was cancelled, by the FIRST signal.
	It("reports the first signal even when qrsh then exits 0, forwarding every signal", func() {
		dir := GinkgoT().TempDir()
		script := filepath.Join(dir, "qrsh")
		Expect(os.WriteFile(script, []byte("#!/bin/sh\n"+
			"n=0\n"+
			"trap 'n=$((n+1)); echo INT >> "+dir+"/got; [ $n -ge 2 ] && exit 0' INT\n"+
			"touch "+dir+"/started\n"+
			"i=0; while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done\n"), 0o755)).To(Succeed())
		sigs := make(chan os.Signal, 2)
		done := make(chan int, 1)
		go func() {
			defer GinkgoRecover()
			code, err := launch.Supervise(exec.Command(script), sigs)
			Expect(err).NotTo(HaveOccurred())
			done <- code
		}()
		Eventually(filepath.Join(dir, "started"), 5*time.Second).Should(BeAnExistingFile())
		sigs <- syscall.SIGHUP
		Eventually(func() ([]byte, error) { return os.ReadFile(filepath.Join(dir, "got")) }, 5*time.Second).
			Should(ContainSubstring("INT"))
		sigs <- syscall.SIGTERM
		Eventually(done, 10*time.Second).Should(Receive(Equal(128 + 1)))
		Expect(os.ReadFile(filepath.Join(dir, "got"))).To(Equal([]byte("INT\nINT\n")))
	})

	It("reports a qrsh that cannot be started as an error", func() {
		_, err := launch.Supervise(exec.Command(filepath.Join(GinkgoT().TempDir(), "missing")), make(chan os.Signal))
		Expect(err).To(MatchError(ContainSubstring("starting")))
	})
})
