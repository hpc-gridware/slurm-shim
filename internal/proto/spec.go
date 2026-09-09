package proto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
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
	// GPUEnvVar is the device-visibility variable name each rank's granted
	// devices are published as (REQ-GPU-004). Resolved once on the master from
	// gpu.vendor so the stepper never loads config and a per-node config skew
	// cannot make a rank disagree with `srun --dry-run`. Empty means
	// EnvCUDADevices, which keeps a pre-AMD stepper's behaviour unchanged.
	GPUEnvVar string `json:"gpu_env_var,omitempty"`
}

// DeviceIDs are one rank's granted device ids. They marshal as JSON numbers when
// every id is numeric and as strings otherwise, and unmarshal from either form.
//
// This exists for one reason: the stepper runs from srun's own binary path on
// each exec host, and the installer copies that binary per node, so a partial or
// rolling upgrade can pair a new srun with an older stepper. Device ids were
// numbers before UUID support, and a bare type change fails that pairing with
// "cannot unmarshal string into Go struct field ... of type int" on every
// multi-node GPU step, naming neither the cause nor the version skew. A numeric
// site -- every site that predates UUID ids -- keeps the old wire bytes exactly,
// so only a site that has opted into UUID ids needs its steppers upgraded too.
type DeviceIDs []string

// MarshalJSON emits numbers when every id is a canonical integer, else strings.
func (d DeviceIDs) MarshalJSON() ([]byte, error) {
	if d == nil {
		return []byte("null"), nil
	}
	for _, id := range d {
		n, err := strconv.Atoi(id)
		if err != nil || strconv.Itoa(n) != id {
			return json.Marshal([]string(d))
		}
	}
	nums := make([]int, len(d))
	for i, id := range d {
		nums[i], _ = strconv.Atoi(id)
	}
	return json.Marshal(nums)
}

// UnmarshalJSON accepts either a number array (an older srun) or a string array.
func (d *DeviceIDs) UnmarshalJSON(b []byte) error {
	var raw []any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw == nil {
		*d = nil
		return nil
	}
	out := make(DeviceIDs, len(raw))
	for i, v := range raw {
		switch t := v.(type) {
		case string:
			out[i] = t
		case float64:
			out[i] = strconv.FormatInt(int64(t), 10)
		default:
			return fmt.Errorf("device id %d: unsupported JSON type %T", i, v)
		}
	}
	*d = out
	return nil
}

// RankSpec is one rank's placement and per-rank Table B environment.
type RankSpec struct {
	Rank     int       `json:"rank"`      // SLURM_PROCID
	Local    int       `json:"local"`     // SLURM_LOCALID
	NodeID   int       `json:"node_id"`   // SLURM_NODEID
	Cpuset   string    `json:"cpuset"`    // affinity mask ("" = unset)
	GPUs     DeviceIDs `json:"gpus"`      // devices for StepSpec.GPUEnvVar (none when empty)
	EnvDelta []string  `json:"env_delta"` // per-rank Table B KEY=VALUE overrides
	// StdoutFile / StderrFile are host-resolved %-pattern paths; "" streams the
	// rank's output back over the channel instead of writing a file.
	StdoutFile string `json:"stdout_file"`
	StderrFile string `json:"stderr_file"`
}

// Device-visibility environment variables. The shim writes exactly one of
// EnvCUDADevices / EnvROCRDevices per rank and removes the others from the rank
// environment, because they are consumed by different layers of the ROCm stack:
// ROCR_VISIBLE_DEVICES filters and renumbers the agent list first, and
// CUDA_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES then index into that already
// filtered list. Absolute ids in both therefore select the wrong devices, or
// none, with no diagnostic. GPU_DEVICE_ORDINAL is the OpenCL-level equivalent.
const (
	EnvCUDADevices      = "CUDA_VISIBLE_DEVICES"
	EnvROCRDevices      = "ROCR_VISIBLE_DEVICES"
	EnvHIPDevices       = "HIP_VISIBLE_DEVICES"
	EnvGPUDeviceOrdinal = "GPU_DEVICE_ORDINAL"

	// EnvCUDADeviceOrder selects what an index in CUDA_VISIBLE_DEVICES refers to.
	// It is deliberately NOT in any drop list: it does not select devices, it
	// decides what the selection means, and removing it would silently change the
	// meaning of the ids for a site that set it on purpose. See the warning in
	// srun for why it is reported instead.
	EnvCUDADeviceOrder = "CUDA_DEVICE_ORDER"

	// OrderPCIBusID makes CUDA enumerate by PCI bus address, which is the order
	// nvidia-smi reports and therefore the order the shim's numeric RSMAP ids are
	// written in. CUDA's own default is FASTEST_FIRST.
	OrderPCIBusID = "PCI_BUS_ID"
)

// EncodeSpec / DecodeSpec (de)serialize a StepSpec for a FrameSpec payload.
func EncodeSpec(s StepSpec) ([]byte, error) { return json.Marshal(s) }

// DecodeSpec parses a StepSpec payload.
func DecodeSpec(b []byte) (StepSpec, error) {
	var s StepSpec
	err := json.Unmarshal(b, &s)
	return s, err
}
