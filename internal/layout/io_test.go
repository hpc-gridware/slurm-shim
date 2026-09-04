package layout_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

func sample() *layout.Layout {
	return &layout.Layout{
		SchemaVersion: layout.SchemaVersion,
		ShimVersion:   "0.1.0-dev",
		CreatedUnix:   1754481600,
		Job:           layout.Job{JobID: 4711, Name: "train-llm", TaskPolicy: "gpu"},
		Nodes: []layout.Node{
			{Index: 0, Host: "node001", Slots: 8, GPUs: []int{0, 1}, IsMaster: true},
			{Index: 1, Host: "node002", Slots: 8, GPUs: []int{0, 1}},
		},
		Tasks:      layout.Tasks{NTasks: 4, CPUsPerTask: 4, PerNode: []int{2, 2}},
		Rendezvous: layout.Rendezvous{MasterAddr: "node001", MasterPort: 24711},
		Launcher:   "qrsh-inherit",
	}
}

var _ = Describe("StateDirFor [REQ-FAB-010]", func() {
	It("joins the state dir under a set TMPDIR", func() {
		dir, err := layout.StateDirFor("/tmp/77.1.all.q")
		Expect(err).NotTo(HaveOccurred())
		Expect(dir).To(Equal("/tmp/77.1.all.q/slurm_shim"))
	})

	It("refuses an unset TMPDIR instead of falling back to a shared /tmp path", func() {
		_, err := layout.StateDirFor("")
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(os.ErrNotExist), "callers treat this as not-inside-a-job")
		Expect(err.Error()).To(ContainSubstring("TMPDIR is not set"))
	})
})

var _ = Describe("Layout IO", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("round-trips through atomic write and read [REQ-LAY-001]", func() {
		Expect(layout.Write(dir, sample())).To(Succeed())
		got, err := layout.Read(filepath.Join(dir, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(sample()))
	})

	It("writes the layout file with mode 0600 [REQ-LAY-004]", func() {
		Expect(layout.Write(dir, sample())).To(Succeed())
		info, err := os.Stat(filepath.Join(dir, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("leaves no temp files behind after a successful write [REQ-LAY-004]", func() {
		Expect(layout.Write(dir, sample())).To(Succeed())
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].Name()).To(Equal(layout.LayoutFile))
	})

	It("creates the state directory if absent [REQ-LAY-004]", func() {
		nested := filepath.Join(dir, "slurm_shim")
		Expect(layout.Write(nested, sample())).To(Succeed())
		_, err := layout.Read(filepath.Join(nested, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects an unknown schema_version [REQ-LAY-005]", func() {
		l := sample()
		l.SchemaVersion = 999
		data, err := json.Marshal(l)
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(dir, layout.LayoutFile)
		Expect(os.WriteFile(path, data, 0o600)).To(Succeed())

		_, err = layout.Read(path)
		var verr layout.ErrSchemaVersion
		Expect(errors.As(err, &verr)).To(BeTrue())
		Expect(verr.Got).To(Equal(999))
		Expect(verr.Want).To(Equal(layout.SchemaVersion))
	})

	It("surfaces a parse error on malformed JSON", func() {
		path := filepath.Join(dir, layout.LayoutFile)
		Expect(os.WriteFile(path, []byte("{not json"), 0o600)).To(Succeed())
		_, err := layout.Read(path)
		Expect(err).To(HaveOccurred())
	})
})
