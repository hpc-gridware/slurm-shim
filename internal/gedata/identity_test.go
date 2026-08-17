package gedata_test

import (
	"context"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("Identity seam [REQ-FAB-004]", func() {
	It("prefers $USER, then $LOGNAME for the user name", func() {
		GinkgoT().Setenv("USER", "alice")
		GinkgoT().Setenv("LOGNAME", "bob")
		id := gedata.RealIdentity{}
		name, uid, _, err := id.User()
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("alice"))
		Expect(uid).To(BeNumerically(">=", 0))
	})

	It("falls back to getent passwd via the Runner when env is unset", func() {
		GinkgoT().Setenv("USER", "")
		GinkgoT().Setenv("LOGNAME", "")
		GinkgoT().Setenv("SGE_O_LOGNAME", "")
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Stdout: []byte("carol:x:1000:1000:Carol:/home/carol:/bin/sh\n")}
		}}
		id := gedata.RealIdentity{Runner: r}
		name, uid, _, err := id.User()
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("carol"))
		Expect(r.Calls[0].Name).To(Equal("getent"))
		Expect(r.Calls[0].Args).To(HaveLen(2))
		Expect(r.Calls[0].Args[0]).To(Equal("passwd"))
		Expect(r.Calls[0].Args[1]).To(Equal(strconv.Itoa(uid)))
	})

	It("falls back to getent hosts when the Go resolver misses [REQ-FAB-004]", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Stdout: []byte("10.0.0.11  node001 node001.cluster.local\n")}
		}}
		id := gedata.RealIdentity{Runner: r}
		ip, err := id.LookupIP(context.Background(), "node001-nonexistent-host.invalid")
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal("10.0.0.11"))
	})
})
