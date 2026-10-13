package gedata

import "strings"

// runKey identifies the records of one job or array task in a qacct answer.
type runKey struct {
	job  int64
	task int64
}

// peRun gathers what the PE task records of one run say about it, until the
// run's master record arrives.
type peRun struct {
	hosts []string
	tasks int
}

// peTaskRuns collects PE task records per run. Under accounting_summary FALSE
// each `qrsh -inherit` task writes its own record, and qacct prints those
// BEFORE the master record of the same run (verified on OCS 9.1.6: a 4-slot,
// 2-host job printed 1.ocs-worker1, 1.ocs-master, then NONE). Taking the
// pending tasks when the master arrives therefore attributes each task to the
// run it belongs to, even when a requeued job has several runs in one answer.
type peTaskRuns map[runKey]*peRun

// isPETask reports whether a qacct record belongs to a PE task rather than to
// the job itself (whose pe_taskid is NONE or absent).
func isPETask(peTaskID string) bool {
	pt := strings.TrimSpace(peTaskID)
	return pt != "" && pt != "NONE"
}

// add records one PE task record for the run.
func (p peTaskRuns) add(k runKey, host string) {
	r := p[k]
	if r == nil {
		r = &peRun{}
		p[k] = r
	}
	r.tasks++
	r.hosts = appendHost(r.hosts, host)
}

// take returns the distinct hosts of a run, master host first, and the number of
// PE task records seen for it, then forgets the run so a later run of the same
// job starts empty.
func (p peTaskRuns) take(k runKey, masterHost string) ([]string, int) {
	hosts := appendHost(nil, masterHost)
	r := p[k]
	if r == nil {
		return hosts, 0
	}
	delete(p, k)
	for _, h := range r.hosts {
		hosts = appendHost(hosts, h)
	}
	return hosts, r.tasks
}

// appendHost adds host unless it is empty or already listed.
func appendHost(hosts []string, host string) []string {
	host = strings.TrimSpace(host)
	if host == "" {
		return hosts
	}
	for _, h := range hosts {
		if h == host {
			return hosts
		}
	}
	return append(hosts, host)
}

// grantedPE normalizes qacct's granted_pe, which prints NONE for a job that
// ran without a parallel environment.
func grantedPE(pe string) string {
	pe = strings.TrimSpace(pe)
	if pe == "NONE" {
		return ""
	}
	return pe
}
