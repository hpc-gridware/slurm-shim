package install

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// UninstallOptions steer PlanUninstall.
type UninstallOptions struct {
	// InTree reports whether a path (a starter_method, a start_proc_args)
	// lies in the tree being uninstalled. PathIn is the lexical check;
	// TreeMatcher also follows symlinks, so another spelling of the same tree
	// is recognised.
	InTree func(path string) bool
	// PEName names a PE install created under another name than the default
	// (uninstall --pe), for an install that left no record.
	PEName string
}

// PathIn returns a lexical InTree for prefix: the path, cleaned, is prefix
// itself or below it.
func PathIn(prefix string) func(string) bool {
	prefix = filepath.Clean(prefix)
	return func(p string) bool {
		p = filepath.Clean(command(p))
		return p == prefix || strings.HasPrefix(p, prefix+string(filepath.Separator))
	}
}

// TreeMatcher is InTree for the tree at prefix however it is spelled: a path
// matches when it is under prefix lexically, or when its directory resolves
// (through symlinks) to one under prefix's resolved location.
func TreeMatcher(prefix string) func(string) bool {
	lexical := PathIn(prefix)
	resolved := prefix
	if r, err := filepath.EvalSymlinks(prefix); err == nil {
		resolved = r
	}
	inResolved := PathIn(resolved)
	return func(p string) bool {
		if lexical(p) || inResolved(p) {
			return true
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(command(p))))
		return err == nil && inResolved(filepath.Join(dir, filepath.Base(command(p))))
	}
}

// ShimRef is a slurm-shim install the cluster points at: its prefix and the
// object that shows it.
type ShimRef struct {
	Prefix string
	Object string
}

// ShimInstalls lists the installs the cluster points at, by their starter
// (queue starter_method) or slurm-shim-env (PE start_proc_args).
func ShimInstalls(f Facts) []ShimRef {
	var out []ShimRef
	for _, q := range f.Queues {
		if c := command(q.StarterMethod); filepath.Base(c) == "slurm-shim-starter" {
			out = append(out, ShimRef{filepath.Dir(filepath.Dir(c)), "queue " + q.Name + " starter_method " + q.StarterMethod})
		}
	}
	for _, pe := range f.PEs {
		if c := command(pe.StartProcArgs); filepath.Base(c) == "slurm-shim-env" {
			out = append(out, ShimRef{filepath.Dir(filepath.Dir(c)), "pe " + pe.Name + " start_proc_args " + pe.StartProcArgs})
		}
	}
	return out
}

// command is the executable of a starter_method or start_proc_args value
// (its first field), without a "user@" prefix.
func command(v string) string {
	f := strings.Fields(v)
	if len(f) == 0 {
		return ""
	}
	c := f[0]
	if i := strings.Index(c, "@"); i > 0 && !strings.Contains(c[:i], "/") {
		c = c[i+1:]
	}
	return c
}

// runsAsOtherUser reports whether a start_proc_args or starter value uses GE's
// "user@/path" form, which runs the command as that user (root@ as root).
func runsAsOtherUser(v string) bool {
	c := strings.Fields(v)
	return len(c) > 0 && command(v) != c[0]
}

// PlanUninstall decides how to undo an install. Like MakePlan it is a pure
// function of the cluster's state and the install record (st, nil for an
// install made before install recorded one), so every rule is a unit spec.
//
// It recognises the install by what points into the tree (o.InTree): queues
// whose starter_method is in it, PEs whose start_proc_args is. It never trusts
// the record alone: the queue it deletes must be the one the record names AND
// still look like the shim's, a PE it deletes must point into the tree, and a
// recorded value is restored only onto an object that points into the tree.
// What it cannot undo safely it refuses, naming the fix; uninstall applies
// nothing while any refusal stands.
func PlanUninstall(f Facts, st *State, o UninstallOptions) Plan {
	var p Plan
	inTree := o.InTree

	// The PEs: the shim's own are deleted; a site PE install --force
	// repaired is restored from the record.
	var ours, restored []string
	for _, pe := range f.PEs {
		if !inTree(pe.StartProcArgs) {
			continue
		}
		// A record of start_proc_args means install --force took over a site PE.
		// Other records (the legacy slots raise) leave the PE the shim's own.
		repaired := slices.ContainsFunc(st.replacedAttrs("pe "+pe.Name), func(r Replaced) bool {
			return r.Attr == "start_proc_args"
		})
		switch {
		case repaired:
			restored = append(restored, pe.Name)
		case pe.Name == DefaultPEName || (st != nil && pe.Name == st.CreatedPE) || pe.Name == o.PEName:
			ours = append(ours, pe.Name)
		default:
			hint := "if install created it, re-run with --pe " + pe.Name + "; if install --force replaced " +
				"its start_proc_args, set the site's value back by hand"
			p.Changes = append(p.Changes, Change{
				Kind: ChangeRefused, Object: pe.Name, Attr: "start_proc_args", Old: pe.StartProcArgs,
				Reason: "PE points at this tree and there is no record of what it was before; " + hint,
			})
		}
	}

	created := ""
	if st != nil && st.CreatedQueue == DefaultQueueName {
		if q, ok := f.findQueue(DefaultQueueName); ok {
			if createdByShim(q, ours, inTree) {
				created = q.Name
			} else {
				p.Warnings = append(p.Warnings, "queue "+q.Name+" is kept: the record names it, but it no longer "+
					"looks like the shim's (its starter or pe_list belong to something else)")
			}
		}
	}

	for _, q := range f.Queues {
		if q.Name == created {
			continue // deleted below, with everything it offers
		}
		for _, pe := range ours {
			if contains(q.PEList, pe) {
				p.Changes = append(p.Changes, Change{
					Kind: ChangeRemoveFromPEList, Object: q.Name, Attr: "pe_list", Old: pe,
					Reason: "the PE is deleted below",
				})
			}
		}
		for _, pe := range restored {
			if contains(q.PEList, pe) && st != nil && slices.Contains(st.PEListAdds, PEListAdd{Queue: q.Name, PE: pe}) {
				p.Changes = append(p.Changes, Change{
					Kind: ChangeRemoveFromPEList, Object: q.Name, Attr: "pe_list", Old: pe,
					Reason: "install added it",
				})
			}
		}
		for _, ov := range q.PEListOverrides {
			for _, pe := range ours {
				if slices.Contains(strings.FieldsFunc(ov, overrideSep), pe) {
					p.Changes = append(p.Changes, Change{
						Kind: ChangeRefused, Object: q.Name, Attr: "pe_list", Old: ov,
						Reason: "a per-host pe_list entry offers " + pe + "; remove it by hand (qconf -mq " + q.Name + ")",
					})
				}
			}
		}
		for _, ov := range q.StarterOverrides {
			if _, v, ok := strings.Cut(strings.Trim(ov, "[]"), "="); ok && inTree(v) {
				p.Changes = append(p.Changes, Change{
					Kind: ChangeRefused, Object: q.Name, Attr: "starter_method", Old: ov,
					Reason: "a per-host starter_method points at this tree; remove it by hand (qconf -mq " + q.Name + ")",
				})
			}
		}
		if q.StarterMethod != "" && inTree(q.StarterMethod) {
			old, _ := st.replaced("queue "+q.Name, "starter_method")
			p.Changes = append(p.Changes, Change{
				Kind: ChangeSetStarter, Object: q.Name, Attr: "starter_method", Old: q.StarterMethod, New: old,
				Reason: "back to what the queue had before install",
			})
			if q.Name == DefaultQueueName && created == "" {
				p.Warnings = append(p.Warnings, "queue "+DefaultQueueName+" is unwired but kept: "+
					"there is no record that install created it (delete it by hand with qconf -dq if it is unused)")
			}
		}
	}
	if created != "" {
		p.Changes = append(p.Changes, Change{Kind: ChangeDeleteQueue, Object: created, Reason: "install created it"})
	}
	for _, pe := range ours {
		p.Changes = append(p.Changes, Change{Kind: ChangeDeletePE, Object: pe, Reason: "the shim's own PE"})
	}
	if st != nil && st.CreatedComplex == ShimComplex {
		if c, ok := f.findComplex(ShimComplex); ok && strings.EqualFold(c.Requestable, "FORCED") {
			p.Changes = append(p.Changes, Change{Kind: ChangeDeleteComplex, Object: ShimComplex, Reason: "install created it"})
		}
	}
	for _, pe := range restored {
		for _, r := range st.replacedAttrs("pe " + pe) {
			if runsAsOtherUser(r.Old) {
				p.Changes = append(p.Changes, Change{
					Kind: ChangeRefused, Object: pe, Attr: r.Attr, New: r.Old,
					Reason: "the recorded value runs as another user; check it and set it by hand",
				})
				continue
			}
			p.Changes = append(p.Changes, Change{
				Kind: ChangeSetPEAttr, Object: pe, Attr: r.Attr, New: r.Old,
				Reason: "back to what the PE had before install --force",
			})
		}
	}
	return p
}

// createdByShim reports whether q still looks like the queue install created:
// no starter or this tree's, and nothing but the shim's PEs on offer. A queue
// the site rebuilt under the same name fails it and is kept.
func createdByShim(q gedata.Queue, ours []string, inTree func(string) bool) bool {
	if q.StarterMethod != "" && !inTree(q.StarterMethod) {
		return false
	}
	for _, pe := range q.PEList {
		if !contains(ours, pe) {
			return false
		}
	}
	return len(q.PEListOverrides) == 0
}

func overrideSep(r rune) bool {
	return r == '[' || r == ']' || r == '=' || r == ' ' || r == ','
}

// References lists what in the cluster still points into the tree: a queue's
// starter_method (default or per host) or a PE's start_proc_args. Uninstall
// removes no file while anything does -- a job started there would fail.
func References(f Facts, inTree func(string) bool) []string {
	var out []string
	for _, q := range f.Queues {
		if q.StarterMethod != "" && inTree(q.StarterMethod) {
			out = append(out, "queue "+q.Name+" starter_method "+q.StarterMethod)
		}
		for _, ov := range q.StarterOverrides {
			if _, v, ok := strings.Cut(strings.Trim(ov, "[]"), "="); ok && inTree(v) {
				out = append(out, "queue "+q.Name+" starter_method "+ov)
			}
		}
	}
	for _, pe := range f.PEs {
		if inTree(pe.StartProcArgs) {
			out = append(out, "pe "+pe.Name+" start_proc_args "+pe.StartProcArgs)
		}
	}
	return out
}

// BusyJobs returns the jobs, pending or running, that still use what the
// uninstall plan removes: a PE it deletes (requested by name or by a wildcard
// such as "slurm*"), the queue it deletes (requested or granted), or the
// complex it deletes. Grid Engine refuses to delete a PE any job references,
// but only after the queues were already unwired, and it deletes a queue or a
// complex a pending job needs -- so uninstall checks first and changes nothing.
func BusyJobs(p Plan, uses []gedata.JobUse) []string {
	var pes []string
	queues, complexes := map[string]bool{}, map[string]bool{}
	for _, c := range p.Changes {
		switch c.Kind {
		case ChangeDeletePE:
			pes = append(pes, c.Object)
		case ChangeDeleteQueue:
			queues[c.Object] = true
		case ChangeDeleteComplex:
			complexes[c.Object] = true
		}
	}
	var busy []string
	for _, u := range uses {
		hit := false
		for _, pe := range pes {
			if ok, _ := path.Match(u.PE, pe); ok || u.PE == pe {
				hit = true
			}
		}
		for _, q := range u.Queues {
			hit = hit || queues[q]
		}
		for _, r := range u.Resources {
			hit = hit || complexes[r]
		}
		if hit && !contains(busy, u.ID) {
			busy = append(busy, u.ID)
		}
	}
	sort.Strings(busy)
	return busy
}

// drainScriptsRel are the optional reference drain scripts a payload carries.
var drainScriptsRel = []string{"share/drain-load-sensor.sh", "share/drain-qmod-helper.sh"}

// modulefilesRel is where Expose writes the modulefile, relative to the prefix.
const modulefilesRel = "share/modulefiles/slurm-shim"

// IsShimTree reports, as an error, why prefix is not a slurm-shim install
// tree: it must hold bin/slurm-shim as a regular file. Uninstall removes no
// file from a directory that fails it.
func IsShimTree(prefix string) error {
	fi, err := os.Lstat(filepath.Join(prefix, BinaryRel))
	if err != nil {
		return fmt.Errorf("%s is not a slurm-shim install tree (no %s)", prefix, BinaryRel)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a slurm-shim install tree (%s is not a regular file)", prefix, BinaryRel)
	}
	return nil
}

// TreeFiles lists the files install put under prefix that are still there,
// relative to it: the binary, starter, hook, record and config pointer, the
// drain scripts, each command link that points at slurm-shim, and the
// modulefiles Expose wrote. Nothing else is ever listed, so nothing else is ever removed. It
// reads through os.Root, so a symlink leading out of the tree is not followed.
func TreeFiles(prefix string) ([]string, error) {
	root, err := os.OpenRoot(prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return treeFiles(root), nil
}

func treeFiles(root *os.Root) []string {
	var out []string
	for _, rel := range append([]string{BinaryRel, StarterRel, HookRel, StateRel, config.PointerRel}, drainScriptsRel...) {
		if fi, err := root.Lstat(rel); err == nil && fi.Mode().IsRegular() {
			out = append(out, rel)
		}
	}
	for _, c := range Commands {
		rel := filepath.Join("bin", c)
		if target, err := root.Readlink(rel); err == nil && target == "slurm-shim" {
			out = append(out, rel)
		}
	}
	if dir, err := root.Open(modulefilesRel); err == nil {
		entries, _ := dir.ReadDir(-1)
		_ = dir.Close()
		for _, e := range entries {
			rel := filepath.Join(modulefilesRel, e.Name())
			if !e.Type().IsRegular() {
				continue
			}
			if data, err := root.ReadFile(rel); err == nil && strings.Contains(string(data), modulefileMarker) {
				out = append(out, rel)
			}
		}
	}
	return out
}

// RemoveTree removes what install put under prefix (TreeFiles), then each of
// the tree's directories left empty, then prefix itself if empty. Everything
// goes through os.Root, so a symlinked directory inside the tree cannot lead
// it elsewhere. profileD is removed when it is exactly the file Expose writes
// for the tree under one of its spellings.
func RemoveTree(prefix, profileD string, spellings []string) ([]string, error) {
	var removed []string
	if profileD != "" {
		if data, err := os.ReadFile(profileD); err == nil {
			for _, sp := range spellings {
				if string(data) == profileDBody(sp) {
					if err := os.Remove(profileD); err != nil {
						return removed, err
					}
					removed = append(removed, profileD)
					break
				}
			}
		}
	}
	root, err := os.OpenRoot(prefix)
	if err != nil {
		return removed, err
	}
	defer func() { _ = root.Close() }()
	for _, rel := range treeFiles(root) {
		if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed = append(removed, filepath.Join(prefix, rel))
	}
	// Deepest first; only real directories, and only empty ones go.
	for _, rel := range []string{modulefilesRel, filepath.Dir(modulefilesRel), "share", "etc", "bin"} {
		if fi, err := root.Lstat(rel); err == nil && fi.IsDir() && root.Remove(rel) == nil {
			removed = append(removed, filepath.Join(prefix, rel))
		}
	}
	if fi, err := os.Lstat(prefix); err == nil && fi.IsDir() && os.Remove(prefix) == nil {
		removed = append(removed, prefix)
	}
	return removed, nil
}
