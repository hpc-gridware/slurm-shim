/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package fabricator

import (
	"context"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// discoverHostSlots records the slots each granted host offers any job, so a
// step can tell whether the job holds the host whole (cpubind.Holding,
// CS-2945): Grid Engine knows, the CPU topology does not -- a site may give
// an SMT host a slot per core or per thread. A job that asked for its hosts
// exclusively (exclusive_complex) holds each one whole whatever its slots.
// Best-effort: on failure the hosts stay unknown and steps fall back to
// counting CPUs, with one warning.
func discoverHostSlots(ctx context.Context, r gedata.Runner, cfg *config.Config, jobID string, hosts []nodeInfo) []string {
	if r == nil || len(hosts) == 0 {
		return nil
	}
	if cfg.ExclusiveComplex != "" && jobID != "" && jobID != "0" {
		if v, ok, err := gedata.RequestedResource(ctx, r, jobID, cfg.ExclusiveComplex); err == nil && ok && v > 0 {
			for i := range hosts {
				hosts[i].hostSlots = hosts[i].Slots
			}
			return nil
		}
	}
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.FQDN
	}
	capacity, err := gedata.HostSlotCapacity(ctx, r, names)
	if err != nil {
		return []string{"cannot read the slots of the job's hosts (" + err.Error() +
			"); CPU pinning decides by counting CPUs"}
	}
	for i, h := range hosts {
		short, _, _ := strings.Cut(h.Name, ".")
		hosts[i].hostSlots = capacity[short]
	}
	return nil
}
