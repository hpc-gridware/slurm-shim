package layout_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"

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
			{Index: 0, Host: "node001", Slots: 8, GPUs: []string{"0", "1"}, IsMaster: true},
			{Index: 1, Host: "node002", Slots: 8, GPUs: []string{"0", "1"}},
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

var _ = Describe("layout schema migration [REQ-LAY-005]", func() {
	// v1 wrote GPU device ids as JSON numbers; v2 writes strings so a device can
	// be named by its stable UUID. Upgrading the shim binary while a job is
	// running must not strand that job: srun reads the layout its own fabricator
	// wrote earlier, and failing here would abort a running allocation.
	v1Doc := `{
	  "schema_version": 1,
	  "shim_version": "old",
	  "job": {"job_id": 4711, "name": "train"},
	  "nodes": [
	    {"index": 0, "host": "node001", "slots": 8, "gpus": [0, 1], "is_master": true},
	    {"index": 1, "host": "node002", "slots": 8, "gpus": [2]}
	  ],
	  "tasks": {"ntasks": 2, "cpus_per_task": 4, "per_node": [1, 1],
	            "rank_map": [{"rank": 0, "node": 0, "local": 0, "gpus": [0, 1], "cpuset": "0-3"}]},
	  "rendezvous": {"master_addr": "node001", "master_port": 24711},
	  "launcher": "qrsh-inherit"
	}`

	writeV1 := func() string {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, layout.LayoutFile)
		Expect(os.WriteFile(path, []byte(v1Doc), 0o600)).To(Succeed())
		return path
	}

	It("reads a v1 layout and renders its device ids as strings", func() {
		got, err := layout.Read(writeV1())
		Expect(err).NotTo(HaveOccurred())
		Expect(got.SchemaVersion).To(Equal(layout.SchemaVersion))
		Expect(got.Nodes[0].GPUs).To(Equal([]string{"0", "1"}))
		Expect(got.Nodes[1].GPUs).To(Equal([]string{"2"}))
		Expect(got.Tasks.RankMap[0].GPUs).To(Equal([]string{"0", "1"}))
	})

	It("preserves every non-device field across the migration", func() {
		got, err := layout.Read(writeV1())
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Job.JobID).To(Equal(int64(4711)))
		Expect(got.Nodes[0].Host).To(Equal("node001"))
		Expect(got.Nodes[0].Slots).To(Equal(8))
		Expect(got.Nodes[0].IsMaster).To(BeTrue())
		Expect(got.Tasks.NTasks).To(Equal(2))
		Expect(got.Tasks.CPUsPerTask).To(Equal(4))
		Expect(got.Tasks.RankMap[0].Cpuset).To(Equal("0-3"))
		Expect(got.Rendezvous.MasterPort).To(Equal(24711))
		Expect(got.Launcher).To(Equal("qrsh-inherit"))
	})

	It("still rejects a schema from the future", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, layout.LayoutFile)
		Expect(os.WriteFile(path, []byte(`{"schema_version": 99}`), 0o600)).To(Succeed())
		_, err := layout.Read(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("99"))
	})

	It("still rejects a schema older than the oldest readable one", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, layout.LayoutFile)
		Expect(os.WriteFile(path, []byte(`{"schema_version": 0}`), 0o600)).To(Succeed())
		_, err := layout.Read(path)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("layout migration fidelity and strictness [REQ-LAY-005]", func() {
	writeDoc := func(doc map[string]any) string {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, layout.LayoutFile)
		b, err := json.Marshal(doc)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(path, b, 0o600)).To(Succeed())
		return path
	}

	// downgrade renders the current sample as a v1 document: schema_version 1 and
	// device ids as numbers. Deriving it from sample() rather than hand-typing a
	// literal means no field can silently drop out of the migration, including
	// fields added to Layout years from now.
	downgrade := func() map[string]any {
		b, err := json.Marshal(sample())
		Expect(err).NotTo(HaveOccurred())
		var doc map[string]any
		Expect(json.Unmarshal(b, &doc)).To(Succeed())
		doc["schema_version"] = 1
		toNumbers := func(obj any) {
			m := obj.(map[string]any)
			ids, ok := m["gpus"].([]any)
			if !ok {
				return
			}
			nums := make([]any, len(ids))
			for i, id := range ids {
				n, err := strconv.Atoi(id.(string))
				Expect(err).NotTo(HaveOccurred())
				nums[i] = n
			}
			m["gpus"] = nums
		}
		for _, n := range doc["nodes"].([]any) {
			toNumbers(n)
		}
		if tasks, ok := doc["tasks"].(map[string]any); ok {
			if ranks, ok := tasks["rank_map"].([]any); ok {
				for _, r := range ranks {
					toNumbers(r)
				}
			}
		}
		return doc
	}

	It("loses no field at all when migrating a full v1 layout", func() {
		got, err := layout.Read(writeDoc(downgrade()))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(sample()))
	})

	It("migrates a CPU-only layout, the one most likely to be mid-flight", func() {
		doc := downgrade()
		for _, n := range doc["nodes"].([]any) {
			n.(map[string]any)["gpus"] = nil
		}
		got, err := layout.Read(writeDoc(doc))
		Expect(err).NotTo(HaveOccurred())
		for _, n := range got.Nodes {
			Expect(n.GPUs).To(BeEmpty())
		}
	})

	It("migrates a layout whose gpus key is absent entirely", func() {
		doc := downgrade()
		for _, n := range doc["nodes"].([]any) {
			delete(n.(map[string]any), "gpus")
		}
		_, err := layout.Read(writeDoc(doc))
		Expect(err).NotTo(HaveOccurred())
	})

	It("is idempotent on ids that are already strings", func() {
		doc := downgrade()
		doc["nodes"].([]any)[0].(map[string]any)["gpus"] = []any{"0", "1"}
		got, err := layout.Read(writeDoc(doc))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Nodes[0].GPUs).To(Equal([]string{"0", "1"}))
	})

	// Strictness. A migration that quietly produced a GPU job with no device masks
	// would be the silent-wrong-device failure the string ids exist to prevent, so
	// every unexpected shape is an error rather than a skip.
	DescribeTable("refuses a malformed v1 document rather than guessing",
		func(mutate func(map[string]any), wants string) {
			doc := downgrade()
			mutate(doc)
			_, err := layout.Read(writeDoc(doc))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wants))
		},
		Entry("a device id that is neither number nor string",
			func(d map[string]any) {
				d["nodes"].([]any)[0].(map[string]any)["gpus"] = []any{true}
			}, "expected a number or string"),
		Entry("a fractional device id",
			func(d map[string]any) {
				d["nodes"].([]any)[0].(map[string]any)["gpus"] = []any{1.5}
			}, "not an integer device id"),
		Entry("a gpus value that is not an array",
			func(d map[string]any) {
				d["nodes"].([]any)[0].(map[string]any)["gpus"] = "0,1"
			}, "expected an array"),
		Entry("a node that is not an object",
			func(d map[string]any) { d["nodes"] = []any{"node001"} }, "expected an object"),
		Entry("a nodes value that is not an array",
			func(d map[string]any) { d["nodes"] = 3.0 }, "nodes: expected an array"),
	)
})
