package gedata

import (
	"context"
	"fmt"
	"strings"
)

// HoldKinds maps every held job's id to its hold kind (HoldUser, else
// HoldAdmin for an operator or system hold), from `qstat -xml -s hu|ho|hs`.
//
// It is the fallback for OCS 9.0, whose `qstat -xml -j` leaves the hold ranges
// (JB_ja_u_h_ids, JB_ja_o_h_ids, JB_ja_s_h_ids) out entirely, so a held job's
// detail says nothing about why it is held (verified on OCS 9.0.10; 9.1
// prints them). Callers ask only when a job's state shows a hold that its
// detail does not explain, which on 9.1 does not happen. A job held by both a
// user and an operator reads as a user hold, as JobInfo.Hold does.
func HoldKinds(ctx context.Context, r Runner) (map[string]string, error) {
	kinds := map[string]string{}
	for _, f := range []struct{ filter, kind string }{
		{"hu", HoldUser}, {"ho", HoldAdmin}, {"hs", HoldAdmin},
	} {
		out, errOut, exit, err := r.Run(ctx, "qstat", "-xml", "-u", "*", "-s", f.filter)
		if err != nil {
			return nil, fmt.Errorf("qstat -s %s: %w", f.filter, err)
		}
		if exit != 0 {
			return nil, fmt.Errorf("qstat -s %s: exit %d: %s", f.filter, exit, strings.TrimSpace(string(errOut)))
		}
		rows, err := ParseQstatXML(out)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, seen := kinds[row.JobID]; !seen {
				kinds[row.JobID] = f.kind
			}
		}
	}
	return kinds, nil
}
