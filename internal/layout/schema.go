// Package layout defines the canonical allocation-state file (spec section 5)
// and its atomic IO. Every other component reads allocation truth from this
// file rather than re-parsing PE_HOSTFILE (REQ-LAY-001).
package layout

// SchemaVersion is the layout schema this build writes and accepts. Readers
// reject any other value (REQ-LAY-005).
const SchemaVersion = 1

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
	Index          int    `json:"index"`
	Host           string `json:"host"`
	FQDN           string `json:"fqdn"`
	IP             string `json:"ip"`
	Slots          int    `json:"slots"`
	ProcessorRange string `json:"processor_range"`
	GPUs           []int  `json:"gpus"`
	IsMaster       bool   `json:"is_master"`
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
	Rank   int    `json:"rank"`
	Node   int    `json:"node"`
	Local  int    `json:"local"`
	GPUs   []int  `json:"gpus"`
	Cpuset string `json:"cpuset"`
}

// Rendezvous holds the derived master address and port (Table A28).
type Rendezvous struct {
	MasterAddr string `json:"master_addr"`
	MasterPort int    `json:"master_port"`
}
