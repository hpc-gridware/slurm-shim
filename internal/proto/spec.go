package proto

import (
	"encoding/base64"
	"encoding/json"
)

// Envelope is the routing information passed to the stepper in its argv. It
// carries NO secrets and NO environment (SI-35): argv is world-readable via
// /proc/<pid>/cmdline on shared hosts. The auth token travels separately in the
// stepper's environment; the full StepSpec (including the job environment)
// travels over the authenticated control channel.
type Envelope struct {
	JobID  int64  `json:"job_id"`
	StepID int    `json:"step_id"`
	Host   string `json:"host"`    // this stepper's host name
	NodeID int    `json:"node_id"` // this host's index in the step nodelist
	Dial   string `json:"dial"`    // control-channel address to dial (host:port)
}

// EncodeEnvelope renders an envelope as a single base64(JSON) argv token.
func EncodeEnvelope(e Envelope) (string, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// DecodeEnvelope parses a base64(JSON) argv token.
func DecodeEnvelope(s string) (Envelope, error) {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Envelope{}, err
	}
	var e Envelope
	err = json.Unmarshal(data, &e)
	return e, err
}

// StepSpec is the authoritative step description delivered to the stepper over
// the control channel after authentication. It holds the full environment
// (SI-10: never via qrsh -V) and this host's rank list.
type StepSpec struct {
	// Env is the base environment applied to every rank (KEY=VALUE), already
	// filtered per --export. Per-rank Table B values are layered on top.
	Env []string `json:"env"`
	// Command is the user command and its argv, passed through verbatim.
	Command []string `json:"command"`
	// Chdir is the working directory for ranks ("" = the stepper's cwd).
	Chdir string `json:"chdir"`
	// Label prefixes each output line with "<rank>: " (srun -l). The demux does
	// the prefixing; the stepper only frames output with the rank id.
	Label bool `json:"label"`
	// ExportNone starts each rank from a minimal environment (REQ-STP-003).
	ExportNone bool `json:"export_none"`
	// Ranks are the ranks this host runs.
	Ranks []RankSpec `json:"ranks"`
}

// RankSpec is one rank's placement and per-rank Table B environment.
type RankSpec struct {
	Rank     int      `json:"rank"`      // SLURM_PROCID
	Local    int      `json:"local"`     // SLURM_LOCALID
	NodeID   int      `json:"node_id"`   // SLURM_NODEID
	Cpuset   string   `json:"cpuset"`    // affinity mask ("" = unset)
	GPUs     []int    `json:"gpus"`      // CUDA_VISIBLE_DEVICES ("" when empty)
	EnvDelta []string `json:"env_delta"` // per-rank Table B KEY=VALUE overrides
	// StdoutFile / StderrFile are host-resolved %-pattern paths; "" streams the
	// rank's output back over the channel instead of writing a file.
	StdoutFile string `json:"stdout_file"`
	StderrFile string `json:"stderr_file"`
}

// EncodeSpec / DecodeSpec (de)serialize a StepSpec for a FrameSpec payload.
func EncodeSpec(s StepSpec) ([]byte, error) { return json.Marshal(s) }

// DecodeSpec parses a StepSpec payload.
func DecodeSpec(b []byte) (StepSpec, error) {
	var s StepSpec
	err := json.Unmarshal(b, &s)
	return s, err
}
