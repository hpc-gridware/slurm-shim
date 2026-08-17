package proto_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestProto(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Proto Suite")
}
