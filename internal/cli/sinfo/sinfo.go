// Package sinfo implements the sinfo shim (spec sec. 7.10): a minimal partition
// listing derived from the config partitions map. --version is handled by the
// dispatcher; everything beyond the default listing is unsupported.
package sinfo

import (
	"fmt"
	"io"
	"sort"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// Run is the sinfo entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, warnings, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "sinfo: error: %v\n", err)
		return 2
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "sinfo: warning: %s\n", w)
	}
	return run(cfg, args, stdout, stderr)
}

func run(cfg *config.Config, args []string, stdout, _ io.Writer) int {
	_ = args // no flags beyond --version (handled upstream) are supported (SI-32)
	fmt.Fprintln(stdout, "PARTITION AVAIL TIMELIMIT NODES STATE NODELIST")

	names := make([]string, 0, len(cfg.Partitions))
	for name := range cfg.Partitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Node counts and states require GE queries the shim does not perform in
		// this mode; the row lists the partition with neutral placeholders.
		fmt.Fprintf(stdout, "%s up infinite 0 n/a -\n", name)
	}
	return 0
}
