// Package config loads the shim's YAML configuration (spec section 9 as amended
// by SI-37). A missing file yields built-in defaults (REQ-CFG-001); a malformed
// file is a hard error; unknown keys warn and are ignored for forward
// compatibility (REQ-CFG-002).
package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvVar names the environment variable that overrides the config search path.
const EnvVar = "SLURM_SHIM_CONFIG"

// DefaultPath is the fixed location searched when EnvVar is unset.
const DefaultPath = "/etc/slurm-shim/config.yaml"

// Duration wraps time.Duration so YAML scalars like "30s" parse via
// time.ParseDuration (REQ-CFG-002).
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Partition maps a SLURM partition to a GE queue and PE plus the slot rule used
// to translate sbatch geometry (SI-04).
type Partition struct {
	Queue string `yaml:"queue"`
	PE    string `yaml:"pe"`
	Slots string `yaml:"slots"`
}

// PE holds per-PE task semantics.
type PE struct {
	TaskPolicy string `yaml:"task_policy"`
}

// GPU holds GPU discovery and isolation settings (spec section 10).
type GPU struct {
	Discovery   string `yaml:"discovery"`
	Isolation   string `yaml:"isolation"`
	GresComplex string `yaml:"gres_complex"`
	// Bind selects what a task sees when no --gpus-per-task is given.
	// "none" (default) matches SLURM: the node's whole grant stays visible to
	// every task, which frameworks selecting by local rank (JAX, torchrun)
	// require. "per-task" restores the shim's legacy even split.
	Bind string `yaml:"bind"`
}

// Config is the full shim configuration.
type Config struct {
	CompatVersion    string   `yaml:"compat_version"`
	Launcher         string   `yaml:"launcher"`
	StrictFlags      bool     `yaml:"strict_flags"`
	Standalone       string   `yaml:"standalone"`
	KillOnBadExit    bool     `yaml:"kill_on_bad_exit"`
	KillWait         Duration `yaml:"kill_wait"`
	MasterInterface  string   `yaml:"master_interface"`
	ExportMasterAddr bool     `yaml:"export_master_addr"`
	MasterPortBase   int      `yaml:"master_port_base"`
	MasterPortRange  int      `yaml:"master_port_range"`
	QstatTimeout     Duration `yaml:"qstat_timeout"`
	MemoryComplex    string   `yaml:"memory_complex"`
	EmitCPUsPerTask  bool     `yaml:"emit_cpus_per_task"`

	// Control-channel and launch settings (D-1, SI-37).
	LaunchRamp    int      `yaml:"launch_ramp"`
	LaunchTimeout Duration `yaml:"launch_timeout"`
	ControlPort   string   `yaml:"control_port"`
	PingInterval  Duration `yaml:"ping_interval"`
	PingDeadline  Duration `yaml:"ping_deadline"`
	OrphanGrace   Duration `yaml:"orphan_grace"`
	QacctDeadline Duration `yaml:"qacct_deadline"`

	JobNameSanitize   bool   `yaml:"job_name_sanitize"`
	HookMissingEnv    string `yaml:"hook_missing_env"`
	DefaultTaskPolicy string `yaml:"default_task_policy"`
	// WrapperMode selects how sbatch injects fabrication (SI-57). false (default)
	// submits the user script as-is (the PE start_proc_args hook fabricates);
	// true submits a shim wrapper that runs the fabricator then execs the stored
	// original script verbatim.
	WrapperMode bool `yaml:"wrapper_mode"`
	// WrapperSpoolDir is where wrapper-mode stores the verbatim original script
	// and the generated wrapper. It MUST be on a shared filesystem visible on the
	// exec hosts and persist for the job's lifetime, because the wrapper execs the
	// stored original by path at run time (SI-57). Empty (default) stores the copy
	// next to the user's script (assumed already on shared storage).
	WrapperSpoolDir string `yaml:"wrapper_spool_dir"`

	// DefaultPartition is used by sbatch when no partition is given, mirroring
	// SLURM's DEFAULT partition. (srun has no partition concept in the shim -- it
	// is the in-allocation task launcher.) Empty (default) keeps the "no partition
	// specified" error so a site must opt in.
	DefaultPartition string               `yaml:"default_partition"`
	Partitions       map[string]Partition `yaml:"partitions"`
	PartitionAliases map[string]string    `yaml:"partition_aliases"`
	PEs              map[string]PE        `yaml:"pes"`
	GPU              GPU                  `yaml:"gpu"`
}

// Default returns the built-in configuration (spec section 9 defaults, SI-37).
func Default() *Config {
	return &Config{
		CompatVersion:     "24.05.0",
		Launcher:          "qrsh-inherit",
		StrictFlags:       false,
		Standalone:        "reject",
		KillOnBadExit:     true,
		KillWait:          Duration{30 * time.Second},
		MasterInterface:   "",
		ExportMasterAddr:  false,
		MasterPortBase:    20000,
		MasterPortRange:   10000,
		QstatTimeout:      Duration{5 * time.Second},
		MemoryComplex:     "h_vmem",
		EmitCPUsPerTask:   false,
		LaunchRamp:        64,
		LaunchTimeout:     Duration{60 * time.Second},
		ControlPort:       "",
		PingInterval:      Duration{10 * time.Second},
		PingDeadline:      Duration{30 * time.Second},
		OrphanGrace:       Duration{3 * time.Minute},
		QacctDeadline:     Duration{30 * time.Second},
		JobNameSanitize:   true,
		HookMissingEnv:    "continue",
		DefaultTaskPolicy: "node",
		GPU: GPU{
			Discovery:   "qstat-gres",
			Isolation:   "shim",
			GresComplex: "gpu",
			Bind:        "none",
		},
	}
}

// Load resolves the config path per the search order (EnvVar, then DefaultPath)
// and parses it. A missing file at either location yields defaults with no
// warnings (REQ-CFG-001).
func Load() (*Config, []string, error) {
	path := os.Getenv(EnvVar)
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return Parse(data)
}

// Parse overlays a YAML document onto the defaults. It returns warnings for
// unrecognized top-level keys and a hard error for malformed content or an
// invalid duration (REQ-CFG-002).
func Parse(data []byte) (*Config, []string, error) {
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}

	var warnings []string
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(data, &raw); err == nil {
		known := knownKeys()
		for k := range raw {
			if !known[k] {
				warnings = append(warnings, fmt.Sprintf("unknown config key %q ignored", k))
			}
		}
	}
	return cfg, warnings, nil
}

// knownKeys is the set of recognized top-level YAML keys, derived from the
// Config struct tags so it cannot drift from the schema.
func knownKeys() map[string]bool {
	ks := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if name != "" && name != "-" {
			ks[name] = true
		}
	}
	return ks
}
