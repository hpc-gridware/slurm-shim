package installcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

// runVerify is the README Quickstart's step 3, automated: submit a 2-node job
// through the freshly wired PE with no hook line, and check that the fabricated
// environment reached it. It runs as the invoking user via qsub directly, so it
// does not depend on the shim's own commands being on PATH yet.
func runVerify(ctx context.Context, cfg *config.Config, plan install.Plan, stdout, stderr io.Writer) int {
	part, ok := cfg.Partitions[plan.DefaultPartition]
	if !ok {
		fmt.Fprintln(stderr, "verify: no default partition to test")
		return 1
	}
	dir, err := os.MkdirTemp("", "slurm-shim-verify-")
	if err != nil {
		fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := filepath.Join(dir, "verify.sh")
	out := filepath.Join(dir, "verify.out")
	body := "#!/bin/bash\n" +
		"echo HOSTS=$(scontrol show hostnames 2>/dev/null | tr '\\n' ' ')\n" +
		"echo NSLURM=$(env | grep -c '^SLURM_')\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}
	// Put the shim's bin first so scontrol inside the job resolves to it.
	env := "PATH=" + filepath.Join(filepath.Dir(plan.PE.StartProcArgs)) + ":" + os.Getenv("PATH")
	r := gedata.ExecRunner{}
	o, e, exit, err := r.Run(ctx, "qsub", "-terse", "-sync", "y", "-q", part.Queue, "-pe", part.PE, "2",
		"-o", out, "-j", "y", "-v", env, script)
	if err != nil || exit != 0 {
		fmt.Fprintf(stderr, "verify: qsub failed (exit %d): %s%s\n", exit, strings.TrimSpace(string(e)), errText(err))
		return 1
	}
	_ = o
	time.Sleep(time.Second) // let the output file land on a shared filesystem
	data, err := os.ReadFile(out)
	if err != nil {
		fmt.Fprintf(stderr, "verify: job produced no output at %s: %v\n", out, err)
		return 1
	}
	text := string(data)
	hosts := strings.Fields(strings.TrimPrefix(firstLine(text, "HOSTS="), "HOSTS="))
	nslurm := strings.TrimPrefix(firstLine(text, "NSLURM="), "NSLURM=")
	fmt.Fprintf(stdout, "verify     2-node job ran: %d host(s) from scontrol show hostnames, %s SLURM_* variables\n", len(hosts), nslurm)
	if len(hosts) != 2 || nslurm == "0" || nslurm == "" {
		fmt.Fprintln(stderr, "verify: FAILED: the job did not see the fabricated environment; check the plan above and `slurm-shim doctor`")
		return 1
	}
	fmt.Fprintln(stdout, "verify     OK")
	return 0
}

func firstLine(text, prefix string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}
