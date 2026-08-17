package gedata_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGedata(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GEData Suite")
}
