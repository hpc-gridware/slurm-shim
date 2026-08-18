package srun

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

var _ = Describe("srun control-channel address [REQ-RUN-012]", func() {
	// bindAddr/dialAddr take an explicit remote flag: the caller sets it only when
	// a real tight-integration launcher places a task off the master node.
	s := &supervisor{lay: &layout.Layout{Rendezvous: layout.Rendezvous{MasterAddr: "master"}}}

	It("binds loopback when steppers stay on-box", func() {
		Expect(s.bindAddr(false)).To(Equal("127.0.0.1:0"))
	})

	It("binds all interfaces when steppers run off-box", func() {
		Expect(s.bindAddr(true)).To(Equal("0.0.0.0:0"))
	})

	It("keeps the loopback listener address for a local (non-remote) run", func() {
		// A multi-node layout over the local launcher must still dial loopback.
		Expect(s.dialAddr("127.0.0.1:35131", false)).To(Equal("127.0.0.1:35131"))
	})

	It("advertises the master's routable host with the listener port when remote", func() {
		// The remote stepper must reach the master, not its own loopback.
		Expect(s.dialAddr("0.0.0.0:35131", true)).To(Equal("master:35131"))
	})

	It("falls back to the master node host when MasterAddr is empty", func() {
		m := &supervisor{lay: &layout.Layout{Nodes: []layout.Node{{Host: "master-fqdn"}}}}
		Expect(m.dialAddr("0.0.0.0:40000", true)).To(Equal("master-fqdn:40000"))
	})

	It("brackets an IPv6 master host", func() {
		m := &supervisor{lay: &layout.Layout{Rendezvous: layout.Rendezvous{MasterAddr: "::1"}}}
		Expect(m.dialAddr("[::]:5000", true)).To(Equal("[::1]:5000"))
	})

	It("returns the listener unchanged if it has no parseable port", func() {
		Expect(s.dialAddr("no-port-here", true)).To(Equal("no-port-here"))
	})
})

var _ = Describe("srun stepIsRemote gate", func() {
	master := plan.StepNode{Host: "master", LayoutIndex: 0}
	worker := plan.StepNode{Host: "worker1", LayoutIndex: 1}

	remoteFor := func(nodes []plan.StepNode, slaveIsQrsh bool) bool {
		s := &supervisor{plan: &plan.StepPlan{Nodes: nodes}}
		return s.stepIsRemote(slaveIsQrsh)
	}

	It("is never remote under the local launcher, even multi-node", func() {
		Expect(remoteFor([]plan.StepNode{master, worker}, false)).To(BeFalse())
	})

	It("is local when the only node is the master (layout index 0)", func() {
		Expect(remoteFor([]plan.StepNode{master}, true)).To(BeFalse())
	})

	It("is remote for a single non-master node (the -w <worker> case)", func() {
		// This is the bug the node-count gate missed: len(Nodes)==1 but off-box.
		Expect(remoteFor([]plan.StepNode{worker}, true)).To(BeTrue())
	})

	It("is remote for a multi-node step that includes off-master nodes", func() {
		Expect(remoteFor([]plan.StepNode{master, worker}, true)).To(BeTrue())
	})
})
