// Package config loads the shim's YAML configuration (spec section 9 as amended
// by SI-37). A missing file yields built-in defaults (REQ-CFG-001); a malformed
// file is a hard error; unknown keys warn and are ignored for forward
// compatibility (REQ-CFG-002).
package config

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
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

// Allocation-rule override policies for qsub -par. "auto" probes the cluster
// once per invocation and emits the rule when the client supports it; "never"
// restores the pre-9.1.5 behavior (Grid Engine's PE places the nodes); "always"
// skips the probe, trading the fork for a hard failure on a cluster without -par.
const (
	OverrideAuto   = "auto"
	OverrideNever  = "never"
	OverrideAlways = "always"
)

// Partition maps a SLURM partition to a GE queue and PE plus the slot rule used
// to translate sbatch geometry (SI-04).
type Partition struct {
	Queue string `yaml:"queue"`
	PE    string `yaml:"pe"`
	Slots string `yaml:"slots"`
	// AllocationRuleOverride opts this partition out of (or into) the qsub -par
	// allocation-rule override, winning over the global setting. Empty inherits.
	// Set it to "never" on a partition whose PE allocation_rule IS the site
	// policy -- a $pe_slots "single-node" partition, say -- so an explicit
	// --nodes does not spread a job the PE was chosen to keep together.
	AllocationRuleOverride string `yaml:"allocation_rule_override"`
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
	// AllocationRuleOverride is the site-wide policy for emitting qsub -par:
	// OverrideAuto (default) probes the cluster and emits when supported,
	// OverrideNever never emits, OverrideAlways skips the probe. A partition may
	// override it. knownKeys() picks this field up by reflection.
	AllocationRuleOverride string `yaml:"allocation_rule_override"`
	EmitCPUsPerTask        bool   `yaml:"emit_cpus_per_task"`

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
		CompatVersion:          "24.05.0",
		Launcher:               "qrsh-inherit",
		StrictFlags:            false,
		Standalone:             "reject",
		KillOnBadExit:          true,
		KillWait:               Duration{30 * time.Second},
		MasterInterface:        "",
		ExportMasterAddr:       false,
		MasterPortBase:         20000,
		MasterPortRange:        10000,
		QstatTimeout:           Duration{5 * time.Second},
		MemoryComplex:          "h_vmem",
		AllocationRuleOverride: OverrideAuto,
		EmitCPUsPerTask:        false,
		LaunchRamp:             64,
		LaunchTimeout:          Duration{60 * time.Second},
		ControlPort:            "",
		PingInterval:           Duration{10 * time.Second},
		PingDeadline:           Duration{30 * time.Second},
		OrphanGrace:            Duration{3 * time.Minute},
		QacctDeadline:          Duration{30 * time.Second},
		JobNameSanitize:        true,
		HookMissingEnv:         "continue",
		DefaultTaskPolicy:      "node",
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
	warnings = append(warnings, validate(cfg)...)
	return cfg, warnings, nil
}

// validate reports value-level problems the schema itself cannot express. It only
// ever warns.
//
// Nothing here is a hard error, deliberately. config.Load is called by every
// command, so a fatal return takes down squeue, sinfo, srun and -- worst --
// slurm-shim-env, which is the PE start_proc_args hook: a non-zero exit there puts
// the queue instance into E state for every user on the host. A single partition's
// typo must not have that reach. The problems below are all scoped to one
// partition, so they fail at the point of use instead: computeSlots rejects a bad
// slots rule for the submission that actually names that partition.
//
// Partitions are visited in name order so the warnings are stable across runs.
func validate(cfg *Config) []string {
	var warnings []string
	if m := strings.TrimSpace(cfg.AllocationRuleOverride); !validOverride(m) {
		warnings = append(warnings, fmt.Sprintf(
			"unknown allocation_rule_override %q ignored; using %q", m, OverrideAuto))
		cfg.AllocationRuleOverride = OverrideAuto
	}
	names := make([]string, 0, len(cfg.Partitions))
	for name := range cfg.Partitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := cfg.Partitions[name]
		if w := slotsRuleWarning(name, p.Slots); w != "" {
			warnings = append(warnings, w)
		}
		if m := strings.TrimSpace(p.AllocationRuleOverride); m != "" && !validOverride(m) {
			warnings = append(warnings, fmt.Sprintf(
				"unknown allocation_rule_override %q on partition %q ignored", m, name))
			p.AllocationRuleOverride = ""
			cfg.Partitions[name] = p
		}
	}
	return warnings
}

// slotsRuleWarning describes a slots rule that cannot yield a positive slot count,
// or "" when the rule is usable. ParseSlotsRule is the authority; this only turns
// its error into a warning naming the partition.
func slotsRuleWarning(partition, rule string) string {
	if _, _, err := ParseSlotsRule(rule); err != nil {
		return fmt.Sprintf("partition %q: %v; submissions to it will fail", partition, err)
	}
	return ""
}

// ParseSlotsRule interprets a partition's slots rule. perTask reports the default
// rule (empty or "per-task"), where the slot count follows the request's geometry;
// otherwise n is the fixed count the site pinned. One parser so the load-time
// warning and the submit-time error cannot disagree.
func ParseSlotsRule(rule string) (n int, perTask bool, err error) {
	rule = strings.TrimSpace(rule)
	if rule == "" || rule == "per-task" {
		return 0, true, nil
	}
	n, err = strconv.Atoi(rule)
	if err != nil {
		return 0, false, fmt.Errorf("slots rule %q is neither an integer nor \"per-task\"", rule)
	}
	if n < 1 {
		return 0, false, fmt.Errorf("slots rule %q must be at least 1", rule)
	}
	return n, false, nil
}

func validOverride(m string) bool {
	switch m {
	case OverrideAuto, OverrideNever, OverrideAlways:
		return true
	}
	return false
}

// AllocationRuleMode resolves the effective override policy for a partition: its
// own setting when it has one, else the site-wide one.
func (c *Config) AllocationRuleMode(p Partition) string {
	if m := strings.TrimSpace(p.AllocationRuleOverride); m != "" {
		return m
	}
	if m := strings.TrimSpace(c.AllocationRuleOverride); m != "" {
		return m
	}
	return OverrideAuto
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
