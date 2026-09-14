package gedata

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("systemdSetting (execd_params ENABLE_SYSTEMD)", func() {
	It("treats an absent parameter as systemd on", func() {
		disabled, set := systemdSetting([]string{"KEEP_ACTIVE=TRUE"})
		Expect(set).To(BeFalse())
		Expect(disabled).To(BeFalse())
	})

	It("reads FALSE in any case, and 0", func() {
		for _, v := range []string{"FALSE", "false", "0"} {
			disabled, set := systemdSetting([]string{"ENABLE_SYSTEMD=" + v})
			Expect(set).To(BeTrue(), v)
			Expect(disabled).To(BeTrue(), v)
		}
	})

	It("reads TRUE as enabled", func() {
		disabled, set := systemdSetting([]string{"ENABLE_SYSTEMD=TRUE"})
		Expect(set).To(BeTrue())
		Expect(disabled).To(BeFalse())
	})

	It("finds it inside an unsplit host-configuration value", func() {
		// The library splits the global execd_params but keeps a host
		// configuration's value as one string.
		disabled, set := systemdSetting([]string{"KEEP_ACTIVE=TRUE,ENABLE_SYSTEMD=FALSE USAGE_COLLECTION=PDC"})
		Expect(set).To(BeTrue())
		Expect(disabled).To(BeTrue())
	})

	It("treats NONE as no local parameters", func() {
		Expect(hasParams([]string{"NONE"})).To(BeFalse())
		Expect(hasParams(nil)).To(BeFalse())
		Expect(hasParams([]string{"ENABLE_SYSTEMD=FALSE"})).To(BeTrue())
	})
})
