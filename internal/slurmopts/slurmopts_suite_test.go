package slurmopts_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSlurmopts(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Slurmopts Suite")
}
