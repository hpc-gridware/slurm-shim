package submit

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// Resources are the requests that name cluster resources beyond geometry,
// time, memory and GPUs: licenses (-L), node features (-C), a whole node
// (--exclusive), hosts to use (-w) or avoid (-x), and local disk (--tmp).
type Resources struct {
	Licenses      string   // -L value, as given
	Constraint    string   // -C value, as given
	Exclusive     bool     // --exclusive, bare or =node-like
	ExclusiveMode string   // --exclusive=user|mcs|topo: not translated, warned
	Nodelist      []string // -w hosts, expanded
	Exclude       []string // -x hosts, expanded
	Tmp           string   // --tmp, GE-formatted ("10G"), "" unset
}

// NeedsCatalog reports whether resolving the request reads the cluster's
// complex table: license, feature and --exclusive names are checked against it.
func (r Resources) NeedsCatalog() bool {
	return r.Licenses != "" || r.Constraint != "" || r.Exclusive
}

// Grant is the translation of a Resources request: the `-l` entries to add,
// whether `qsub -w e` can still judge the job, and what the user should hear.
type Grant struct {
	Entries []string
	// Unverifiable names why `qsub -w e` must not judge the job, "" when it
	// may: -w e cannot see a load-sensor value (a non-consumable license
	// count), and it counts an exclusive complex held by a running job as
	// taken, so both would turn a job that only has to wait into a refusal.
	Unverifiable string
	Warnings     []string
}

// ResolveResources translates r into `-l` entries. cat is the cluster's
// complex table (nil when r.NeedsCatalog() is false); slots is the job's total
// slot count, which a per-slot license must divide by.
//
// Everything that cannot be expressed exactly is refused rather than
// approximated: a license or feature request that silently changes meaning
// runs a job on the wrong hosts or without its license.
func ResolveResources(cfg *config.Config, cat gedata.Catalog, r Resources, slots int) (Grant, error) {
	var g Grant
	if err := g.addLicenses(cfg, cat, r.Licenses, slots); err != nil {
		return Grant{}, err
	}
	if err := g.addFeatures(cfg, cat, r.Constraint); err != nil {
		return Grant{}, err
	}
	g.addExclusive(cfg, cat, r)
	if err := g.addHosts(r.Nodelist, r.Exclude); err != nil {
		return Grant{}, err
	}
	if r.Tmp != "" {
		if cfg.TmpComplex == "" {
			g.Warnings = append(g.Warnings, "--tmp ignored: no tmp_complex is configured for local disk")
		} else {
			g.Entries = append(g.Entries, cfg.TmpComplex+"="+r.Tmp)
		}
	}
	return g, nil
}

// ParseExclusive reads an --exclusive value: bare, "node" or "exclusive" ask
// for whole nodes; any other mode (user, mcs, topo) is returned to be warned.
func ParseExclusive(val string) (whole bool, mode string) {
	switch strings.ToLower(val) {
	case "", "set", "node", "exclusive":
		return true, ""
	}
	return false, val
}

// Resolve is ResolveResources plus the per-CPU or per-GPU memory scaling
// (memKind "mem-per-cpu" or "mem-per-gpu"; anything else leaves req.Mem as
// it is) for every command that creates an allocation. The complex table is
// read once, and only when a license, feature, --exclusive or per-unit
// memory needs it. nodes is the job's pinned node count, 0 when unknown.
func Resolve(ctx context.Context, runner gedata.Runner, cfg *config.Config, res Resources,
	req *Request, memKind string, slots, nodes int) (Grant, error) {
	perUnit := req.Mem != "" && (memKind == "mem-per-cpu" || memKind == "mem-per-gpu")
	var cat gedata.Catalog
	if res.NeedsCatalog() || perUnit {
		var err error
		if cat, err = gedata.Complexes(ctx, runner); err != nil && res.NeedsCatalog() {
			return Grant{}, fmt.Errorf("reading the cluster's complexes to check licenses/features: %v", err)
		}
	}
	g, err := ResolveResources(cfg, cat, res, slots)
	if err != nil {
		return Grant{}, err
	}
	if perUnit && cfg.MemoryComplex != "" {
		v, warn, err := ScalePerUnitMemory(cfg, cat, *req, memKind, slots, nodes)
		if err != nil {
			return Grant{}, err
		}
		req.Mem = v
		if warn != "" {
			g.Warnings = append(g.Warnings, warn)
		}
	}
	return g, nil
}

// addLicenses maps -L name[:n][,name[:n]...] onto license complexes.
//
// The count's meaning follows the complex's consumable scope, read from the
// cluster: JOB debits it once per job however many hosts the job spans
// (verified on OCS 9.1.6 with a 2-host job), so it is SLURM's meaning exactly.
// A per-slot consumable debits it per slot, which is exact only when the
// count divides by the slots. A non-consumable count is a load value (a
// license-server sensor): Grid Engine checks it but reserves nothing.
func (g *Grant) addLicenses(cfg *config.Config, cat gedata.Catalog, spec string, slots int) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	if strings.Contains(spec, "|") {
		return fmt.Errorf("--licenses=%s: either-or license lists cannot be expressed in Grid Engine; request one license", spec)
	}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, count := item, 1
		if c := strings.LastIndexByte(item, ':'); c >= 0 {
			n, err := strconv.Atoi(item[c+1:])
			if err != nil || n < 1 {
				return fmt.Errorf("--licenses: invalid count in %q", item)
			}
			name, count = item[:c], n
		}
		cx, err := resolveComplex(cat, cfg.Licenses, name, "license")
		if err != nil {
			return err
		}
		if !cx.Numeric() {
			return fmt.Errorf("license %q: complex %s is %s, not a count", name, cx.Name, cx.Type)
		}
		switch strings.ToUpper(cx.Consumable) {
		case gedata.ConsumablePerJob:
		case gedata.ConsumablePerSlot:
			if slots < 1 || count%slots != 0 {
				return fmt.Errorf("license %q: complex %s is consumed per slot, and %d license(s) do not divide "+
					"over the job's %d slot(s); ask your administrator to make it consumable JOB", name, cx.Name, count, slots)
			}
			count /= slots
		case gedata.ConsumableNone:
			g.Unverifiable = "license " + name + " is a load-sensor value"
			g.Warnings = append(g.Warnings, fmt.Sprintf("license %q (complex %s) is checked, not reserved: "+
				"it is not consumable, so jobs starting together can oversubscribe it", name, cx.Name))
		default:
			return fmt.Errorf("license %q: complex %s is consumed per host (%s); a license count needs consumable JOB",
				name, cx.Name, cx.Consumable)
		}
		g.Entries = append(g.Entries, cx.Name+"="+strconv.Itoa(count))
	}
	return nil
}

// addFeatures maps -C a[&b...] onto BOOL complexes. Only conjunctions have a
// Grid Engine form: `|` combines values of ONE complex, so an either-or
// across features, a feature count, a bracketed set or a negation is refused.
func (g *Grant) addFeatures(cfg *config.Config, cat gedata.Catalog, spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	if strings.ContainsAny(spec, "|*[]!()") {
		return fmt.Errorf("--constraint=%s: only features joined by & can be expressed in Grid Engine "+
			"(no |, counts, brackets or !)", spec)
	}
	for _, name := range strings.FieldsFunc(spec, func(r rune) bool { return r == '&' || r == ',' }) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cx, err := resolveComplex(cat, cfg.Features, name, "feature")
		if err != nil {
			return err
		}
		if !strings.EqualFold(cx.Type, "BOOL") {
			return fmt.Errorf("feature %q: complex %s is %s, not BOOL", name, cx.Name, cx.Type)
		}
		if entry := cx.Name + "=true"; !slices.Contains(g.Entries, entry) {
			g.Entries = append(g.Entries, entry)
		}
	}
	return nil
}

// addExclusive requests the site's exclusive-host complex (relop EXCL): a job
// holding it keeps every other job off its hosts (verified on OCS 9.1.6).
// Without one, --exclusive cannot be expressed, and the user is told how to
// get the effect instead.
func (g *Grant) addExclusive(cfg *config.Config, cat gedata.Catalog, r Resources) {
	if r.ExclusiveMode != "" {
		g.Warnings = append(g.Warnings, "--exclusive="+r.ExclusiveMode+
			" ignored: Grid Engine has no per-user or per-MCS node sharing")
		return
	}
	if !r.Exclusive {
		return
	}
	name := cfg.ExclusiveComplex
	if name == "" {
		var found []string
		for _, c := range cat {
			if strings.EqualFold(c.Relop, "EXCL") && c.CanRequest() {
				found = append(found, c.Name)
			}
		}
		if len(found) != 1 {
			g.Warnings = append(g.Warnings, exclusiveWarning(found))
			return
		}
		name = found[0]
	}
	g.Entries = append(g.Entries, name+"=true")
	// Verified on OCS 9.1.6: with any job running on a host, `qsub -w e -l
	// <excl>=true,h=<host>` fails "no suitable queues", and -w v says the
	// exclusive resource "is already in use". SLURM queues such a job.
	g.Unverifiable = "-w e counts a running job's exclusive complex as unavailable"
}

func exclusiveWarning(found []string) string {
	if len(found) > 1 {
		return "--exclusive ignored: the cluster has several exclusive complexes (" +
			strings.Join(found, ", ") + "); set exclusive_complex to the one to use"
	}
	return "--exclusive ignored: the cluster has no exclusive complex (relop EXCL). " +
		"Ask for the width you need (--ntasks-per-node), or have your administrator add one"
}

// addHosts maps -w (use these hosts) and -x (avoid these) onto the hostname
// complex. Both reduce to one expression: the listed hosts minus the excluded
// ones, or every host but the excluded ones.
func (g *Grant) addHosts(nodelist, exclude []string) error {
	switch {
	case len(nodelist) > 0:
		keep := UsableHosts(nodelist, exclude)
		if len(keep) == 0 {
			return fmt.Errorf("--nodelist and --exclude leave no host to run on")
		}
		g.Entries = append(g.Entries, "h="+strings.Join(keep, "|"))
		// -w e refuses a job pinned to hosts that a running job holds
		// exclusively (verified on OCS 9.1.6); SLURM queues it.
		if g.Unverifiable == "" {
			g.Unverifiable = "a running job may hold a named host exclusively, which -w e counts as unavailable"
		}
	case len(exclude) == 1:
		g.Entries = append(g.Entries, "h=!"+exclude[0])
	case len(exclude) > 1:
		g.Entries = append(g.Entries, "h=!("+strings.Join(exclude, "|")+")")
	}
	return nil
}

// hostName is what a Grid Engine host name can be. Anything else in a -w or
// -x list would change the h= expression it lands in: "a,!b" would turn
// "avoid a and b" into "run only on b".
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ExpandHosts reads a -w or -x host list (flag names it in messages): SLURM's
// bracket form, expanded, with every name checked. A host list file is
// refused.
func ExpandHosts(flag, list string) ([]string, error) {
	if list == "" {
		return nil, nil
	}
	if strings.Contains(list, "/") {
		return nil, fmt.Errorf("%s=%s: reading a host list from a file is not supported", flag, list)
	}
	hosts, err := encoders.ExpandNodelist(list)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", flag, err)
	}
	for _, h := range hosts {
		if !hostName.MatchString(h) {
			return nil, fmt.Errorf("invalid %s: %q is not a host name", flag, h)
		}
	}
	return hosts, nil
}

// resolveComplex finds the complex a SLURM license or feature name stands
// for: the configured mapping, else a complex of the same name. The
// "name@server" form of a remote license is looked up by its full name first,
// then by the name before the "@".
func resolveComplex(cat gedata.Catalog, mapping map[string]string, name, kind string) (gedata.Complex, error) {
	candidates := []string{name}
	if base, _, ok := strings.Cut(name, "@"); ok {
		candidates = append(candidates, base)
	}
	for _, n := range candidates {
		target := n
		if m, ok := mapping[n]; ok {
			target = m
		}
		if cx, ok := cat.Lookup(target); ok {
			if !cx.CanRequest() {
				return gedata.Complex{}, fmt.Errorf("%s %q: complex %s is not requestable", kind, name, cx.Name)
			}
			return cx, nil
		}
	}
	return gedata.Complex{}, fmt.Errorf("unknown %s %q: the cluster has no complex of that name "+
		"(map it in the %ss section of the config)", kind, name, kind)
}
