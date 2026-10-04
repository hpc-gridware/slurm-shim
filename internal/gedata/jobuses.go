package gedata

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

// JobUse is what one job, pending or running, asks of the cluster's objects:
// the PE it requested (possibly a wildcard such as "slurm*"), the queues it
// requested hard (-q) or was granted, and the resources it requested hard (-l).
// It is what an uninstall must check before deleting a PE, a queue or a
// complex: Grid Engine refuses to delete a PE any job still references, pending
// jobs included, but deletes a queue or a complex a pending job needs and
// leaves that job unschedulable.
type JobUse struct {
	ID        string
	PE        string
	Queues    []string
	Resources []string
}

// JobUses lists every job the caller may see with what it uses (`qstat -xml -j
// "*"`). Unlike JobAllocations it includes jobs that have not started.
func JobUses(ctx context.Context, r Runner) ([]JobUse, error) {
	out, errOut, exit, err := r.Run(ctx, "qstat", "-xml", "-j", "*")
	if err != nil {
		return nil, fmt.Errorf("qstat -xml -j: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat -xml -j: exit %d: %s", exit, strings.TrimSpace(string(errOut)))
	}
	return ParseJobUsesXML(out)
}

// jobUsesXML reads the request fields of `qstat -xml -j`. OCS 9.1 keeps the
// hard requests in a request set (JB_request_set_list); 9.0 has them directly
// on the job (JB_hard_queue_list, JB_hard_resource_list). Both are read.
type jobUsesXML struct {
	Jobs []struct {
		JobID       string   `xml:"JB_job_number"`
		PE          string   `xml:"JB_pe"`
		HardQueues  []string `xml:"JB_hard_queue_list>destin_ident_list>QR_name"`
		HardRes     []string `xml:"JB_hard_resource_list>qstat_l_requests>CE_name"`
		RequestSets []struct {
			HardQueues []string `xml:"JRS_hard_queue_list>destin_ident_list>QR_name"`
			HardRes    []string `xml:"JRS_hard_resource_list>qstat_l_requests>CE_name"`
		} `xml:"JB_request_set_list>ulong_sublist"`
		Granted []string `xml:"JB_ja_tasks>element>JAT_granted_destin_identifier_list>element>JG_qname"`
	} `xml:"djob_info>element"`
}

// ParseJobUsesXML extracts each job's PE, queues and hard resource requests
// from `qstat -xml -j`. Queue names are cluster queues: a "queue@host" request
// or grant is reduced to its queue.
func ParseJobUsesXML(data []byte) ([]JobUse, error) {
	// No jobs: qstat answers with an unknown_jobs document that is not
	// well-formed XML (see ParseJobAllocationsXML).
	root, err := xmlRootElement(data)
	if err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	if root == "unknown_jobs" {
		return nil, nil
	}
	var doc jobUsesXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	var out []JobUse
	for _, j := range doc.Jobs {
		u := JobUse{ID: strings.TrimSpace(j.JobID), PE: strings.TrimSpace(j.PE)}
		queues, res := j.HardQueues, j.HardRes
		for _, rs := range j.RequestSets {
			queues = append(queues, rs.HardQueues...)
			res = append(res, rs.HardRes...)
		}
		for _, q := range append(queues, j.Granted...) {
			q, _, _ = strings.Cut(strings.TrimSpace(q), "@")
			if q != "" && !contains(u.Queues, q) {
				u.Queues = append(u.Queues, q)
			}
		}
		for _, r := range res {
			if r = strings.TrimSpace(r); r != "" && !contains(u.Resources, r) {
				u.Resources = append(u.Resources, r)
			}
		}
		out = append(out, u)
	}
	return out, nil
}
