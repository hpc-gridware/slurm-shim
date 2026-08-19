package srun_test

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	Expect(err).NotTo(HaveOccurred())
	return n
}

// jaxCoordinatorHost reimplements the hostname half of JAX's
// SlurmCluster.get_coordinator_address (jax/_src/clusters/slurm_cluster.py):
// scan for the first ',' or '['; without a bracket the first token is the host,
// otherwise it is prefix + the digits before the first ',' or '-'. JAX resolves
// its rendezvous host this way from SLURM_STEP_NODELIST, so the shim must emit a
// nodelist whose first host is rank 0's host.
func jaxCoordinatorHost(nodelist string) string {
	ind := strings.IndexAny(nodelist, ",[")
	if ind < 0 {
		return nodelist
	}
	if nodelist[ind] == ',' {
		return nodelist[:ind]
	}
	prefix, suffix := nodelist[:ind], nodelist[ind+1:]
	if i := strings.IndexAny(suffix, ",-"); i >= 0 {
		return prefix + suffix[:i]
	}
	return prefix + suffix
}

var _ = Describe("JAX multi-process contract", func() {
	// Four of the five variables jax.distributed.initialize() requires come from
	// the step; SLURM_JOB_ID is job-level (Table A, covered in the fabricator
	// suite) and is not synthesized by this harness. Auto-detection fires only
	// when ALL five are present (SlurmCluster.is_env_present).
	stepVars := []string{
		"SLURM_STEP_NODELIST", "SLURM_NTASKS", "SLURM_PROCID", "SLURM_LOCALID",
	}

	It("exports the step-level variables JAX auto-detection requires", func() {
		tmp := twoByEight()
		var script strings.Builder
		for _, v := range stepVars {
			script.WriteString("echo " + v + "=${" + v + ":-MISSING}; ")
		}
		sess := runSrun(tmp, "-n", "2", "sh", "-c", script.String())
		Eventually(sess, "60s").Should(gexec.Exit(0))
		out := string(sess.Out.Contents())
		for _, v := range stepVars {
			Expect(out).NotTo(ContainSubstring(v+"=MISSING"), "%s is required by JAX", v)
		}
	})

	It("names step node 0 first in SLURM_STEP_NODELIST (the coordinator invariant)", func() {
		// JAX derives its coordinator from the first host of the step nodelist,
		// and the coordinator service runs in process 0. If the two disagree,
		// every rank hangs until the 300s init timeout. Asserted structurally:
		// rank 0 sits on step node 0, and the nodelist's first host is that node.
		// (Verified against real hostnames on the OCS cluster; this harness runs
		// every node locally, so the reported hostname is the dev machine.)
		tmp := twoByEight()
		sess := runSrun(tmp, "-N", "2", "--ntasks-per-node", "1", "sh", "-c",
			`echo "MARK procid=$SLURM_PROCID nodeid=$SLURM_NODEID list=$SLURM_STEP_NODELIST"`)
		Eventually(sess, "60s").Should(gexec.Exit(0))

		line := regexp.MustCompile(`MARK procid=0 nodeid=(\d+) list=(\S+)`).
			FindStringSubmatch(string(sess.Out.Contents()))
		Expect(line).NotTo(BeNil(), "no rank 0 line in output")
		nodeID, nodelist := line[1], line[2]
		Expect(nodeID).To(Equal("0"), "rank 0 must sit on step node 0")
		// twoByEight's step node 0 is node001, so that is where JAX must rendezvous.
		Expect(jaxCoordinatorHost(nodelist)).To(Equal("node001"),
			"JAX would rendezvous on the wrong host: nodelist %q -> %q",
			nodelist, jaxCoordinatorHost(nodelist))
	})

	DescribeTable("JAX's coordinator parser recovers the first host of a shim nodelist",
		func(hosts []string, want string) {
			Expect(jaxCoordinatorHost(encoders.CompressNodelist(hosts))).To(Equal(want))
		},
		Entry("single host", []string{"node001"}, "node001"),
		Entry("consecutive run", []string{"node001", "node002"}, "node001"),
		Entry("mixed prefixes", []string{"ocs-master", "ocs-worker1", "ocs-worker2"}, "ocs-master"),
		Entry("allocation order, not sorted", []string{"ocs-worker1", "ocs-master"}, "ocs-worker1"),
		Entry("non-consecutive digits", []string{"n8", "n9", "n10"}, "n8"),
		Entry("descending", []string{"node003", "node001"}, "node003"),
	)

	It("never emits a single-element bracket group, which JAX mis-parses", func() {
		// JAX's parser looks for ',' or '-' inside the brackets; a lone element
		// like "n[1]" leaves the closing bracket attached ("n1]"), giving an
		// unresolvable coordinator host and a silent hang until the 300s timeout.
		// Real SLURM never emits that form and neither must the shim.
		for _, hosts := range [][]string{
			{"node1"}, {"n1"}, {"node001"}, {"node10"},
			{"n1", "other"}, {"n1", "n5"}, {"a1", "b2"}, {"n1", "x", "n2"},
		} {
			list := encoders.CompressNodelist(hosts)
			Expect(list).NotTo(MatchRegexp(`\[[^,\-\]]*\]`),
				"nodelist %q contains a single-element bracket group JAX cannot parse", list)
			Expect(jaxCoordinatorHost(list)).To(Equal(hosts[0]))
		}
	})

	It("cannot express a digits-with-suffix hostname to JAX (known upstream limit)", func() {
		// node[01-02]-ib: JAX's parser stops at the '-' and drops the suffix, so
		// it resolves 'node01' instead of 'node01-ib'. Real SLURM emits the same
		// string, so this is a JAX limitation, not a shim divergence -- the recipe
		// documents JAX_COORDINATOR_ADDRESS as the escape hatch.
		list := encoders.CompressNodelist([]string{"node01-ib", "node02-ib"})
		Expect(jaxCoordinatorHost(list)).NotTo(Equal("node01-ib"))
	})

	It("gives every rank a device list its SLURM_LOCALID can index", func() {
		// JAX sets local_device_ids=[SLURM_LOCALID] and indexes into the visible
		// list, so a rank whose LOCALID is past its own device count fails with
		// CUDA_ERROR_INVALID_DEVICE. Guards the SLURM-parity visibility default.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true, GPUs: []int{0, 1, 2, 3}},
		}, []int{4})
		sess := runSrun(tmp, "-n", "4", "sh", "-c",
			`echo "GPU localid=$SLURM_LOCALID devices=$CUDA_VISIBLE_DEVICES"`)
		Eventually(sess, "60s").Should(gexec.Exit(0))

		rows := regexp.MustCompile(`GPU localid=(\d+) devices=(\S*)`).
			FindAllStringSubmatch(string(sess.Out.Contents()), -1)
		Expect(rows).To(HaveLen(4))
		var seen []string
		for _, r := range rows {
			localID, devices := r[1], strings.Split(r[2], ",")
			Expect(len(devices)).To(BeNumerically(">", atoi(localID)),
				"local rank %s cannot index its %d visible device(s)", localID, len(devices))
			seen = append(seen, r[2])
		}
		// SLURM's default leaves the whole grant visible to every task.
		sort.Strings(seen)
		Expect(seen).To(Equal([]string{"0,1,2,3", "0,1,2,3", "0,1,2,3", "0,1,2,3"}))
	})

	It("binds each task to its own device when --gpus-per-task is given", func() {
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true, GPUs: []int{0, 1, 2, 3}},
		}, []int{4})
		sess := runSrun(tmp, "-n", "4", "--gpus-per-task", "1", "sh", "-c",
			`echo "GPU localid=$SLURM_LOCALID devices=$CUDA_VISIBLE_DEVICES"`)
		Eventually(sess, "60s").Should(gexec.Exit(0))

		rows := regexp.MustCompile(`GPU localid=(\d+) devices=(\S*)`).
			FindAllStringSubmatch(string(sess.Out.Contents()), -1)
		Expect(rows).To(HaveLen(4))
		var seen []string
		for _, r := range rows {
			seen = append(seen, r[2])
		}
		sort.Strings(seen)
		Expect(seen).To(Equal([]string{"0", "1", "2", "3"}))
	})

	It("restores the legacy even split under --gpu-bind=per_task", func() {
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 2, IsMaster: true, GPUs: []int{0, 1}},
		}, []int{2})
		sess := runSrun(tmp, "-n", "2", "--gpu-bind", "per_task", "sh", "-c",
			`echo "GPU devices=$CUDA_VISIBLE_DEVICES"`)
		Eventually(sess, "60s").Should(gexec.Exit(0))

		rows := regexp.MustCompile(`GPU devices=(\S*)`).
			FindAllStringSubmatch(string(sess.Out.Contents()), -1)
		var seen []string
		for _, r := range rows {
			seen = append(seen, r[1])
		}
		sort.Strings(seen)
		Expect(seen).To(Equal([]string{"0", "1"}))
	})
})
