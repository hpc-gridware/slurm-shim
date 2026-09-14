package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// hostIsolation is what one exec host contributes to gpu.isolation: cgroup.
type hostIsolation struct {
	Host string
	// Defined is false when the host does not define the GPU RSMAP at all.
	Defined bool
	// Instances counts the RSMAP instances the host defines.
	Instances int
	// Undeclared lists the ids of instances with no devices characteristic.
	Undeclared []string
	// SystemdDisabled is true when execd_params sets ENABLE_SYSTEMD=FALSE.
	SystemdDisabled bool
}

// checkCgroupIsolation verifies, host by host, that Grid Engine will confine
// jobs to their GPUs, and reports the result.
func checkCgroupIsolation(ctx context.Context, r *report, admin *gedata.Admin, complexName string, hosts []string, hostsErr error) {
	if hostsErr != nil {
		r.fail("gpu.isolation cgroup: cannot list exec hosts to verify it: %v", hostsErr)
		return
	}
	if complexName == "" {
		complexName = "gpu"
	}
	var facts []hostIsolation
	for _, host := range hosts {
		instances, found, err := admin.ExecHostResourceMap(ctx, host, complexName)
		if err != nil {
			r.fail("%s: cannot read RSMAP %s: %v", host, complexName, err)
			continue
		}
		fact := hostIsolation{Host: host, Defined: found, Instances: len(instances), Undeclared: devicesUndeclared(instances)}
		if found {
			if off, err := admin.SystemdDisabled(ctx, host); err != nil {
				r.warn("%s: cannot read execd_params to confirm systemd is on: %v", host, err)
			} else {
				fact.SystemdDisabled = off
			}
		}
		facts = append(facts, fact)
	}
	fails, warns, pass := isolationFindings(complexName, facts)
	for _, f := range fails {
		r.fail("%s", f)
	}
	for _, w := range warns {
		r.warn("%s", w)
	}
	if pass != "" {
		r.pass("%s", pass)
	}
}

// devicesUndeclared returns the ids of instances that carry no devices
// characteristic: the ones Grid Engine has no device list to confine a job to.
func devicesUndeclared(instances []gedata.ResourceMapInstance) []string {
	var ids []string
	for _, inst := range instances {
		if strings.TrimSpace(inst.Characteristics["devices"]) == "" {
			ids = append(ids, inst.ID)
		}
	}
	return ids
}

// isolationFindings turns per-host facts into report lines.
//
// Every gap fails OPEN, which is why each one is a FAIL. Grid Engine confines
// a job only to the devices listed on the RSMAP instances it was granted, and
// only through systemd; with no list, or systemd off, it confines nothing.
// Under gpu.isolation: cgroup the shim writes no device variable either, so a
// job on that host can use every GPU on it -- while this report used to print
// PASS "GE masks the devices" without looking.
func isolationFindings(complexName string, hosts []hostIsolation) (fails, warns []string, pass string) {
	defined, instances := 0, 0
	for _, h := range hosts {
		if !h.Defined {
			continue
		}
		defined++
		instances += h.Instances
		if len(h.Undeclared) > 0 {
			fails = append(fails, fmt.Sprintf(
				"%s: RSMAP %s instance(s) %s declare no devices, so nothing confines those GPUs "+
					"and the shim writes no device variable under gpu.isolation cgroup -- jobs there "+
					"can use every GPU on the host (declare devices= per instance, or set gpu.isolation: shim)",
				h.Host, complexName, strings.Join(h.Undeclared, " ")))
		}
		if h.SystemdDisabled {
			fails = append(fails, fmt.Sprintf(
				"%s: execd_params ENABLE_SYSTEMD=FALSE -- device isolation is applied through systemd, "+
					"so jobs there are not confined to their GPUs", h.Host))
		}
	}
	if defined == 0 {
		warns = append(warns, fmt.Sprintf(
			"gpu.isolation is cgroup, but no exec host defines RSMAP %s", complexName))
		return fails, warns, ""
	}
	if len(fails) == 0 {
		pass = fmt.Sprintf("gpu.isolation cgroup: all %d RSMAP %s instance(s) on %d host(s) declare devices, systemd on",
			instances, complexName, defined)
	}
	return fails, warns, pass
}
