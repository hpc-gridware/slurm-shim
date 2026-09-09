// Package config loads the shim's YAML configuration (spec section 9 as amended
// by SI-37). A missing file yields built-in defaults (REQ-CFG-001); a malformed
// file is a hard error; unknown keys warn and are ignored for forward
// compatibility (REQ-CFG-002).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// EnvVar names the environment variable that overrides the config search path.
const EnvVar = "SLURM_SHIM_CONFIG"

// DefaultPath is the fixed fallback location, searched last.
const DefaultPath = "/etc/slurm-shim/config.yaml"

// CellRelPath is where the config lives inside the cell, next to every other
// cluster-wide OCS setting: $SGE_ROOT/$SGE_CELL/common/slurm-shim/config.yaml.
// One copy on a shared root; on a per-node root, the same place the site already
// distributes.
const CellRelPath = "common/slurm-shim/config.yaml"

// SearchPaths returns the config locations in the order Load tries them:
// $SLURM_SHIM_CONFIG if set (alone -- an explicit path is not a search), then
// the cell path when $SGE_ROOT is set, then DefaultPath.
func SearchPaths() []string {
	if p := os.Getenv(EnvVar); p != "" {
		return []string{p}
	}
	var paths []string
	if root := os.Getenv("SGE_ROOT"); root != "" {
		cell := os.Getenv("SGE_CELL")
		if cell == "" {
			cell = "default"
		}
		paths = append(paths, filepath.Join(root, cell, CellRelPath))
	}
	return append(paths, DefaultPath)
}

// CellPath is where the installer writes: the cell path when $SGE_ROOT is set,
// else DefaultPath.
func CellPath() string {
	return SearchPaths()[0]
}

// Render serialises a config as YAML that Parse reads back identically.
func Render(cfg *Config) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// Duration wraps time.Duration so YAML scalars like "30s" parse via
// time.ParseDuration (REQ-CFG-002).
type Duration struct{ time.Duration }

// MarshalYAML renders the duration as the same scalar form UnmarshalYAML
// accepts, so a config the installer writes reads back identically.
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.String(), nil
}

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
	// Vendor selects which device-visibility variable a rank receives:
	// VendorNVIDIA writes CUDA_VISIBLE_DEVICES, VendorAMD writes
	// ROCR_VISIBLE_DEVICES (REQ-GPU-004). Empty means VendorNVIDIA.
	Vendor string `yaml:"vendor"`
}

// GPU vendors, selecting the per-rank device-visibility variable. Exactly one
// variable is ever written, and the other vendor's is removed from the rank
// environment: on ROCm the HIP-level masks index into the list
// ROCR_VISIBLE_DEVICES already filtered, so two masks holding the same absolute
// ids leave the job with a wrong, truncated or empty device set.
const (
	VendorNVIDIA = "nvidia"
	VendorAMD    = "amd"
)

// ValidVendor reports whether v names a supported GPU vendor. Empty is valid and
// means VendorNVIDIA. Matching is case-insensitive: "AMD" is how the brand is
// written everywhere, and refusing every GPU step over the capitalisation would
// be a trap rather than strictness.
func ValidVendor(v string) bool {
	_, _, err := GPU{Vendor: v}.DeviceVars()
	return err == nil
}

// NormalizeVendor trims and lowercases a configured vendor so every consumer can
// compare it directly. An unrecognised value is returned normalised, not blanked,
// so warnings can still name what the site actually wrote.
func NormalizeVendor(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// deviceVars are every device-visibility variable the shim knows about. The
// selected vendor's is written per rank; the rest are removed so an inherited
// value cannot layer on top of it.
var deviceVars = []string{
	proto.EnvCUDADevices, proto.EnvROCRDevices,
	proto.EnvHIPDevices, proto.EnvGPUDeviceOrdinal,
}

// DeviceVars returns the device-visibility variable a rank receives under this
// GPU config and the variables removed from the rank environment (REQ-GPU-004).
//
// This is the single authority. It previously lived in srun, was hand-copied into
// the doctor report and half-restated in ValidVendor, so a vendor added or a drop
// entry changed in one place was silently wrong in the others -- and the report
// could tell an admin something the ranks did not do.
//
// The written variable is itself in the drop set: the per-rank overlay restores it
// for a rank that holds a device and adds nothing to a rank that does not, so
// leaving it would let a device-less rank inherit a mask naming devices the job
// was never granted. The exception is cgroup isolation, where the shim writes
// nothing and that variable is GE's to set.
func (g GPU) DeviceVars() (write string, drop []string, err error) {
	switch NormalizeVendor(g.Vendor) {
	case "", VendorNVIDIA:
		write = proto.EnvCUDADevices
	case VendorAMD:
		write = proto.EnvROCRDevices
	default:
		return "", nil, fmt.Errorf("unknown gpu.vendor %q; expected %q or %q",
			g.Vendor, VendorNVIDIA, VendorAMD)
	}
	for _, name := range deviceVars {
		if name == write && g.Isolation == "cgroup" {
			continue
		}
		drop = append(drop, name)
	}
	return write, drop, nil
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
	// Control-channel listen range. srun binds a port in [base, base+range) so a
	// site can write one firewall rule; base 0 falls back to an ephemeral port,
	// which only works where nothing filters traffic between nodes. Replaces the
	// never-read `control_port`, which could not have supported concurrent steps
	// on one host anyway.
	ControlPortBase  int      `yaml:"control_port_base"`
	ControlPortRange int      `yaml:"control_port_range"`
	PingInterval     Duration `yaml:"ping_interval"`
	PingDeadline     Duration `yaml:"ping_deadline"`
	OrphanGrace      Duration `yaml:"orphan_grace"`
	QacctDeadline    Duration `yaml:"qacct_deadline"`

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
		CompatVersion:    "24.05.0",
		Launcher:         "qrsh-inherit",
		StrictFlags:      false,
		Standalone:       "reject",
		KillOnBadExit:    true,
		KillWait:         Duration{30 * time.Second},
		MasterInterface:  "",
		ExportMasterAddr: false,
		MasterPortBase:   20000,
		MasterPortRange:  10000,
		QstatTimeout:     Duration{5 * time.Second},
		// mem_free, not h_vmem: h_vmem is enforced as a virtual ADDRESS SPACE cap
		// (RLIMIT_AS, verified on OCS 9.1.5), and a CUDA context reserves tens of
		// GB of address space at init, so --mem would fail every GPU job. mem_free
		// is a reported load value, so the request still filters hosts by free
		// memory -- but it is advisory, not a limit (see README).
		MemoryComplex:          "mem_free",
		AllocationRuleOverride: OverrideAuto,
		EmitCPUsPerTask:        false,
		LaunchRamp:             64,
		LaunchTimeout:          Duration{60 * time.Second},
		// 61000-61439: above the usual Linux ephemeral ceiling (32768-60999) so
		// the listener cannot race the source port of an outbound connection,
		// clear of Grid Engine's 6444/6445 and of master_port_base's
		// 20000-29999, and below 61440 -- where JAX's Slurm auto-detect puts its
		// coordinator (SLURM_JOB_ID % 4096 + 61440, i.e. anywhere in
		// 61440-65535). srun binds before it launches rank 0, so an overlap
		// would take the coordinator's port and hang the step until JAX's
		// 300s initialization timeout. That leaves exactly this span, and 440
		// concurrent remote steps on one host is far more than a node runs.
		// SLURM's SrunPortRange is the analogous setting.
		ControlPortBase:   61000,
		ControlPortRange:  440,
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
			Vendor:      VendorNVIDIA,
		},
	}
}

// Load resolves the config path per the search order (EnvVar, then DefaultPath)
// and parses it. A missing file at either location yields defaults with no
// warnings (REQ-CFG-001).
func Load() (*Config, []string, error) {
	cfg, _, warns, err := LoadFrom(SearchPaths())
	return cfg, warns, err
}

// LoadFrom tries each path in order and parses the first that exists,
// returning it so callers (doctor) can say which one was used. "" means none
// existed and the compiled-in defaults apply.
func LoadFrom(paths []string) (*Config, string, []string, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, path, nil, err
		}
		cfg, warns, err := Parse(data)
		return cfg, path, warns, err
	}
	return Default(), "", nil, nil
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
		for _, k := range sortedKeys(raw) {
			if !known[k] {
				warnings = append(warnings, fmt.Sprintf("unknown config key %q ignored", k))
				continue
			}
			warnings = append(warnings, nestedKeyWarnings(k, raw[k])...)
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
	// Normalise once here so no consumer has to trim or fold case again.
	cfg.GPU.Vendor = NormalizeVendor(cfg.GPU.Vendor)
	if v := cfg.GPU.Vendor; !ValidVendor(v) {
		// Warn only, never a hard error: config.Load runs in slurm-shim-env, the
		// PE start_proc_args hook, so a fatal return here would reach every job
		// on the host. The value is refused at the point of use instead, where
		// only a GPU step is affected (see srun gpuEnvVar).
		warnings = append(warnings, fmt.Sprintf(
			"unknown gpu.vendor %q; expected %q or %q; GPU steps will be refused",
			v, VendorNVIDIA, VendorAMD))
	}
	// A non-positive qstat_timeout is not a slower shim, it is one where every
	// qstat call expires before it starts: discovery finds nothing and the job
	// runs without a GPU environment. Warn and fall back rather than accepting a
	// value that disables the thing it configures.
	if cfg.QstatTimeout.Duration <= 0 {
		warnings = append(warnings, fmt.Sprintf(
			"qstat_timeout %s is not positive; using %s",
			cfg.QstatTimeout.Duration, Default().QstatTimeout.Duration))
		cfg.QstatTimeout = Default().QstatTimeout
	}
	warnings = append(warnings, validatePorts(cfg)...)

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
		if name := yamlName(t.Field(i)); name != "" {
			ks[name] = true
		}
	}
	return ks
}

// nestedKeyWarnings reports misspelled keys INSIDE a known block.
//
// knownKeys reflects over top-level Config fields only, so before this a typo one
// level down was accepted in silence: "gpu: {vendr: amd}" left gpu.vendor at its
// default and warned about nothing, which on an AMD site is the wrong device
// variable on every rank from a one-character mistake. That is the failure this
// whole feature exists to prevent, arriving through the config layer.
//
// Two shapes are checked: a block that is a struct (gpu, defaults), and a block
// that is a map of named structs (partitions, pes), where the names are the
// site's own and only the fields inside each are known.
func nestedKeyWarnings(block string, node yaml.Node) []string {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	f, ok := configField(block)
	if !ok {
		return nil
	}
	switch f.Kind() {
	case reflect.Struct:
		return unknownFieldWarnings(block, node, f)
	case reflect.Map:
		var out []string
		elem := f.Elem()
		if elem.Kind() != reflect.Struct {
			return nil
		}
		var sub map[string]yaml.Node
		if node.Decode(&sub) != nil {
			return nil
		}
		for _, name := range sortedKeys(sub) {
			n := sub[name]
			if n.Kind != yaml.MappingNode {
				continue
			}
			out = append(out, unknownFieldWarnings(block+"."+name, n, elem)...)
		}
		return out
	}
	return nil
}

// unknownFieldWarnings names sub-keys of one mapping that t has no field for.
func unknownFieldWarnings(path string, node yaml.Node, t reflect.Type) []string {
	fields := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		if name := yamlName(t.Field(i)); name != "" {
			fields[name] = true
		}
	}
	var sub map[string]yaml.Node
	if node.Decode(&sub) != nil {
		return nil
	}
	var out []string
	for _, k := range sortedKeys(sub) {
		if !fields[k] {
			out = append(out, fmt.Sprintf("unknown config key %q ignored", path+"."+k))
		}
	}
	return out
}

// configField is the type of the Config field with the given yaml name.
func configField(name string) (reflect.Type, bool) {
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		if yamlName(t.Field(i)) == name {
			return t.Field(i).Type, true
		}
	}
	return nil, false
}

func yamlName(f reflect.StructField) string {
	name := strings.Split(f.Tag.Get("yaml"), ",")[0]
	if name == "-" {
		return ""
	}
	return name
}

// sortedKeys keeps warning order stable across runs, since map iteration is not.
func sortedKeys(m map[string]yaml.Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// maxPort is the highest TCP port number.
const maxPort = 65535

// validatePorts keeps the control range inside the port space and warns about
// settings that would silently do nothing.
//
// This matters more than a normal config check: `slurm-shim ports` renders these
// numbers as a firewall rule, so a range running past 65535 does not just waste
// bind attempts, it prints a rule an admin cannot install.
func validatePorts(cfg *Config) []string {
	var warnings []string
	if cfg.ControlPortBase == 0 {
		return nil // documented opt-out: ephemeral port, no rule possible
	}
	if cfg.ControlPortBase < 0 || cfg.ControlPortBase > maxPort {
		warnings = append(warnings, fmt.Sprintf(
			"control_port_base %d is not a valid port; using an ephemeral port, which no "+
				"firewall rule can describe", cfg.ControlPortBase))
		cfg.ControlPortBase, cfg.ControlPortRange = 0, 0
		return warnings
	}
	if cfg.ControlPortBase < 1024 {
		warnings = append(warnings, fmt.Sprintf(
			"control_port_base %d is a privileged port; srun does not run as root, so "+
				"binding will fail", cfg.ControlPortBase))
	}
	if cfg.ControlPortRange <= 0 {
		warnings = append(warnings, fmt.Sprintf(
			"control_port_base %d is set but control_port_range is %d, so srun binds an "+
				"ephemeral port and the base has no effect; set a range (e.g. 2000)",
			cfg.ControlPortBase, cfg.ControlPortRange))
		return warnings
	}
	if cfg.ControlPortBase+cfg.ControlPortRange-1 > maxPort {
		clamped := maxPort - cfg.ControlPortBase + 1
		warnings = append(warnings, fmt.Sprintf(
			"control_port_base %d + control_port_range %d runs past %d; clamping the range "+
				"to %d so the ports and the firewall rule are valid",
			cfg.ControlPortBase, cfg.ControlPortRange, maxPort, clamped))
		cfg.ControlPortRange = clamped
	}
	return warnings
}
