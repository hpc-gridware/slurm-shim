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

// Package layout defines the canonical allocation-state file (spec section 5)
// and its atomic IO. Every other component reads allocation truth from this
// file rather than re-parsing PE_HOSTFILE (REQ-LAY-001).
package layout

// SchemaVersion is the layout schema this build writes. Readers accept this and
// any older version they can migrate (REQ-LAY-005); see Read.
//
// v2 widened GPU device ids from numbers to strings so a device can be named by
// its stable UUID rather than an enumeration index that moves on reboot, driver
// reload, or an AMD compute-partition change.
const SchemaVersion = 2

// MinReadableSchemaVersion is the oldest layout this build can still read. A v1
// layout is migrated in memory by rendering its numeric device ids as strings,
// so upgrading the binary mid-job does not strand an in-flight allocation.
const MinReadableSchemaVersion = 1

// State directory and file names under the per-job $TMPDIR (spec section 11.1).
const (
	StateDir    = "slurm_shim"
	LayoutFile  = "layout.json"
	StepCtrFile = "stepctr"
)

// Layout is the canonical description of a PE allocation (spec section 5.1).
type Layout struct {
	SchemaVersion int        `json:"schema_version"`
	ShimVersion   string     `json:"shim_version"`
	CreatedUnix   int64      `json:"created_unix"`
	Job           Job        `json:"job"`
	Nodes         []Node     `json:"nodes"`
	Tasks         Tasks      `json:"tasks"`
	Rendezvous    Rendezvous `json:"rendezvous"`
	Launcher      string     `json:"launcher"`

	// Hot spares (sbatch --x-spares). Spares are granted hosts kept out of Nodes,
	// so no step places ranks on them until one replaces a lost node: the swap
	// rewrites Nodes in place (the spare takes the lost node's index), moves the
	// spare out of Spares, and records the lost host and the swap. Additive and
	// omitted when empty, so a job without spares writes exactly the v2 file and
	// a reader that ignores these fields still places ranks correctly.
	Spares []Node   `json:"spares,omitempty"`
	Lost   []string `json:"lost,omitempty"`
	Swaps  []Swap   `json:"swaps,omitempty"`
}

// Swap records one spare replacing a lost node.
type Swap struct {
	Lost  string `json:"lost"`
	Spare string `json:"spare"`
	Step  int    `json:"step"`
	Unix  int64  `json:"unix"`
}

// Job carries the job-scoped identity and provenance (Table A sources).
type Job struct {
	JobID       int64  `json:"job_id"`
	ArrayTaskID *int64 `json:"array_task_id"`
	Name        string `json:"name"`
	User        string `json:"user"`
	UID         int    `json:"uid"`
	GID         int    `json:"gid"`
	Queue       string `json:"queue"`
	Partition   string `json:"partition"`
	Account     string `json:"account"`
	SubmitDir   string `json:"submit_dir"`
	SubmitHost  string `json:"submit_host"`
	PEName      string `json:"pe_name"`
	TaskPolicy  string `json:"task_policy"`
	// MemPerNodeMB is the granted per-node memory in MB (A27), derived from the
	// requested memory complex; 0 means unset/omitted.
	MemPerNodeMB int `json:"mem_per_node_mb"`
}

// Node is one allocation host. nodes[0] is the master and index equals array
// position (REQ-LAY-002). ProcessorRange is an opaque GE token (SI-25).
type Node struct {
	Index          int      `json:"index"`
	Host           string   `json:"host"`
	FQDN           string   `json:"fqdn"`
	IP             string   `json:"ip"`
	Slots          int      `json:"slots"`
	ProcessorRange string   `json:"processor_range"`
	GPUs           []string `json:"gpus"`
	IsMaster       bool     `json:"is_master"`
	// HostSlots is the slots the host offers any job (all its queue
	// instances, capped by its own slots limit), or Slots for a job that asked
	// for the host exclusively; 0 when unknown. Slots >= HostSlots means no
	// other job can run there (cpubind.Holding). Additive, omitted when 0.
	HostSlots int `json:"host_slots,omitempty"`
}

// Tasks is the job-level task geometry and default block rank map.
type Tasks struct {
	NTasks      int    `json:"ntasks"`
	CPUsPerTask int    `json:"cpus_per_task"`
	PerNode     []int  `json:"per_node"`
	RankMap     []Rank `json:"rank_map"`
}

// Rank is one entry in the default block-distribution rank map.
type Rank struct {
	Rank   int      `json:"rank"`
	Node   int      `json:"node"`
	Local  int      `json:"local"`
	GPUs   []string `json:"gpus"`
	Cpuset string   `json:"cpuset"`
}

// Rendezvous holds the derived master address and port (Table A28).
type Rendezvous struct {
	MasterAddr string `json:"master_addr"`
	MasterPort int    `json:"master_port"`
}
