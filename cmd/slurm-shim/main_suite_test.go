package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDispatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Dispatch Suite")
}
