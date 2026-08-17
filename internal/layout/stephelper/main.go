// Command stephelper issues exactly one step id via layout.NextStep and prints
// it to stdout. It exists only to drive the multi-process step-counter
// concurrency spec (REQ-TST-005) and is not part of the shipped binary.
package main

import (
	"fmt"
	"os"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: stephelper <counter-path>")
		os.Exit(2)
	}
	id, err := layout.NextStep(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, id)
}
