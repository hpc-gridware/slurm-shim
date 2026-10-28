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

package fabricator_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// A plain mpirun in the job script binds its ranks itself; the job's
// environment tells it not to wherever that would leave the job's CPUs, as
// srun --mpi=pmix does (CS-2945).
var _ = Describe("the OpenMPI binding override in the job environment", func() {
	// fab fabricates a 2-node job holding slots on each node, on a master
	// whose CPU facts are node.
	fab := func(slots string, node cpubind.Node, env map[string]string) *fabricator.Result {
		path := filepath.Join(GinkgoT().TempDir(), "pe_hostfile")
		hostfile := "node001 " + slots + " all.q@node001 UNDEFINED\nnode002 " + slots + " all.q@node002 UNDEFINED\n"
		Expect(os.WriteFile(path, []byte(hostfile), 0o600)).To(Succeed())
		m := map[string]string{"JOB_ID": "267", "JOB_NAME": "mpi", "PE": "make", "PE_HOSTFILE": path}
		for k, v := range env {
			m[k] = v
		}
		res, err := fabricator.Fabricate(fabricator.Options{
			Env:      func(k string) string { return m[k] },
			Config:   config.Default(),
			Identity: fake.Identity{HostnameVal: "node001", UserName: "alice", UID: 1000, GID: 1000},
			NowUnix:  1754481600,
			Node:     node,
		})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	exports := func(res *fabricator.Result) map[string]string {
		m := map[string]string{}
		for _, kv := range res.Exports {
			m[kv.Key] = kv.Value
		}
		return m
	}
	const ompi, prte = "OMPI_MCA_hwloc_base_binding_policy", "PRTE_MCA_hwloc_default_binding_policy"
	all := func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	shared := cpubind.Node{Allowed: all(14), Cores: 14, Online: 14}

	It("exports policy none for a job bound by Grid Engine", func() {
		bound := cpubind.Node{Allowed: []int{2, 3}, Cores: 2, Online: 14}
		got := exports(fab("2", bound, nil))
		Expect(got).To(HaveKeyWithValue(ompi, "none"))
		Expect(got).To(HaveKeyWithValue(prte, "none"))
	})

	It("exports policy none for a job sharing its nodes", func() {
		Expect(exports(fab("2", shared, nil))).To(HaveKeyWithValue(prte, "none"))
	})

	It("leaves OpenMPI's per-core binding to a job owning its nodes", func() {
		got := exports(fab("14", shared, nil))
		Expect(got).NotTo(HaveKey(ompi))
		Expect(got).NotTo(HaveKey(prte))
	})

	It("follows SLURM_CPU_BIND from sbatch", func() {
		Expect(exports(fab("14", shared, map[string]string{"SLURM_CPU_BIND": "none"}))).To(HaveKeyWithValue(ompi, "none"))
		Expect(exports(fab("2", shared, map[string]string{"SLURM_CPU_BIND": "cores"}))).NotTo(HaveKey(ompi))
	})

	It("keeps a policy the user submitted with", func() {
		got := exports(fab("2", shared, map[string]string{prte: "core"}))
		Expect(got).NotTo(HaveKey(ompi))
		Expect(got).NotTo(HaveKey(prte), "the job keeps the user's value, not ours")
	})

	It("adds nothing without CPU facts (non-Linux, golden suites)", func() {
		Expect(exports(fab("2", cpubind.Node{}, nil))).NotTo(HaveKey(ompi))
	})
})
