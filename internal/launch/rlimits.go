package launch

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// perSlotMemoryLimits are the queue rlimits Grid Engine applies PER SLOT, and
// therefore the ones that make the daemon_forks_slaves setting dangerous. Time
// and file limits are deliberately absent: they are not what multiplies, and
// SI-18 is a memory story.
var perSlotMemoryLimits = []string{
	"h_vmem", "s_vmem",
	"h_rss", "s_rss",
	"h_data", "s_data",
	"h_stack", "s_stack",
}

// QueueMemoryLimits names the per-slot memory rlimits the queue actually sets,
// ignoring the INFINITY default. An empty result means the queue imposes none,
// so the SI-18 hazard cannot bite there.
//
// This is what turns SI-18 from a standing notice into a finding. The warning
// used to fire on every step of every job on every cluster -- both branches of
// the daemon_forks_slaves switch warn, so no configuration could silence it --
// while saying nothing about whether the hazard was real. A warning that is
// always printed carries no information, and this one printed into the job's
// own output stream, between the ranks' lines.
func QueueMemoryLimits(ctx context.Context, r gedata.Runner, queue string) []string {
	if r == nil || queue == "" {
		return nil
	}
	// A queue INSTANCE (queue@host) is not a valid argument to qconf -sq.
	if at := strings.IndexByte(queue, '@'); at >= 0 {
		queue = queue[:at]
	}
	stdout, _, exit, err := r.Run(ctx, "qconf", "-sq", queue)
	if err != nil || exit != 0 {
		// Unreadable queue config is NOT evidence of no limits. Say nothing
		// rather than assert safety we did not establish.
		return nil
	}

	want := map[string]bool{}
	for _, k := range perSlotMemoryLimits {
		want[k] = true
	}
	var found []string
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !want[fields[0]] {
			continue
		}
		// GE spells "no limit" as INFINITY. Anything else is a real cap.
		if strings.EqualFold(fields[1], "INFINITY") {
			continue
		}
		found = append(found, fmt.Sprintf("%s=%s", fields[0], fields[1]))
	}
	sort.Strings(found)
	return found
}
