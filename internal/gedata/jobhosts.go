package gedata

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// JobHosts maps a job key (see JobKey) to the exec hosts holding that job's
// tasks, in the order the scheduler granted them. Pass a job id to ask about one
// job, or "" for every job the caller may see.
//
// The source is JAT_granted_destin_identifier_list in `qstat -xml -j`, whose
// JG_qname carries the granted queue instance, host included and untruncated.
//
// The text view `qstat -g t` was used first and is unusable for this; two ways it
// yields wrong hosts were reproduced on a live cluster. Its columns are split on
// whitespace, so a job name containing a space -- which Grid Engine permits, and
// which this shim's own `sbatch --job-name` forwards verbatim -- shifts every
// field, drops the record, and reattaches the continuation lines that follow to
// the PREVIOUS, unrelated job. And it prints the queue instance with precision 30
// unless SGE_LONG_QNAMES is set, so a short queue name plus a fully qualified
// host silently yields a hostname that does not exist. The XML view has neither
// problem: hosts are elements, not columns, and are never truncated.
//
// A job that has not started carries no task element at all and simply does not
// appear in the result (verified on OCS 9.1.5); callers render that as unknown
// rather than as a host.
func JobHosts(ctx context.Context, r Runner, jobID string) (map[string][]string, error) {
	// qstat -j has no -u selector (it ignores one), so the only narrowing
	// available is by job. "*" asks for every job, which is what a listing of all
	// users needs anyway.
	selector := "*"
	if jobID != "" {
		// An array element's hosts live under its job, so ask for the job.
		selector = baseJobID(jobID)
	}
	out, errOut, exit, err := r.Run(ctx, "qstat", "-xml", "-j", selector)
	if err != nil {
		return nil, fmt.Errorf("qstat -xml -j: %w", err)
	}
	stderr := strings.TrimSpace(string(errOut))
	if exit != 0 {
		if stderr == "" {
			stderr = "qstat reported no detail"
		}
		return nil, fmt.Errorf("qstat -xml -j: exit %d: %s", exit, stderr)
	}
	hosts, err := ParseJobHostsXML(out)
	if err != nil {
		// A qstat that exits 0 while writing to stderr has still said something
		// about why its output is unreadable; report it instead of discarding it.
		if stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, stderr)
		}
		return nil, err
	}
	return hosts, nil
}

// baseJobID strips an array suffix, turning "4713_2" into "4713". qstat -j takes
// a job id, not a SLURM array element id.
func baseJobID(jobID string) string {
	if i := strings.IndexByte(jobID, '_'); i >= 0 {
		return jobID[:i]
	}
	return jobID
}

// grantedJobsXML models only the granted-destination subtree of `qstat -xml -j`.
// encoding/xml ignores unmatched elements, so this stays deliberately small,
// mirroring detailedJobXML in gres.go, which reads the granted RESOURCES out of
// the same document.
//
// Field names are stable across the OCS releases the shim supports: they are the
// CULL field names of the JAT/JG objects, which the qstat XSD has carried
// unchanged since 8.x. Compare queues.go, which reasons about the same pinning
// question for the go-clusterscheduler v9.1 package.
type grantedJobsXML struct {
	Jobs []struct {
		JobID string `xml:"JB_job_number"`
		Tasks []struct {
			TaskNumber string `xml:"JAT_task_number"`
			Granted    []struct {
				QHostname string `xml:"JG_qhostname"`
				QName     string `xml:"JG_qname"`
			} `xml:"JAT_granted_destin_identifier_list>element"`
		} `xml:"JB_ja_tasks>element"`
	} `xml:"djob_info>element"`
}

// ParseJobHostsXML extracts the granted hosts per job from `qstat -xml -j`.
//
// Hosts keep the order the scheduler granted them and are never sorted: the
// nodelist encoding is first-seen order (REQ-ENC-002, as amended by SI-41),
// because a sorted encoding desynchronises a derived master address from rank-0
// placement.
func ParseJobHostsXML(data []byte) (map[string][]string, error) {
	// qstat answers a selector that matches nothing with an <unknown_jobs>
	// document whose entries are wrapped in a literal "<>" element. That is not
	// well-formed XML and no unmarshaller can read it, so recognise the root
	// before parsing: "no such job" is an ordinary answer, not a failure.
	root, err := xmlRootElement(data)
	if err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	if root == "unknown_jobs" {
		return map[string][]string{}, nil
	}

	var doc grantedJobsXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}

	hosts := map[string][]string{}
	for _, job := range doc.Jobs {
		id := strings.TrimSpace(job.JobID)
		if id == "" {
			continue
		}
		for _, task := range job.Tasks {
			var list []string
			for _, g := range task.Granted {
				// The QUEUE INSTANCE is the authoritative name form, not
				// JG_qhostname. Both name the same machine, but JG_qhostname can
				// carry the resolved FQDN while the queue instance carries the form
				// the cluster actually uses everywhere else -- including
				// PE_HOSTFILE, and therefore SLURM_JOB_NODELIST. Taking the
				// hostname field made squeue report
				// "shimval-g1.us-central1-b.c.<project>.internal" for a job whose
				// own environment said "shimval-g1" (caught by the e2e suite on a
				// GCP cluster, 2026-09-15). A user deriving a master address from
				// `scontrol show hostnames "$(squeue -h -o %N -j ID)"` would then
				// get names the job itself never uses. The two forms are identical
				// on a short-name cluster, which is why the container fixtures
				// could not show the difference.
				host := hostFromQueueInstance(g.QName)
				if host == "" {
					// Insurance for output that carries no queue instance.
					host = strings.TrimSpace(g.QHostname)
				}
				if host == "" {
					continue
				}
				// One job can hold slots in two queues on the same host, which is
				// one node, not two, and arrives here as two elements.
				if !contains(list, host) {
					list = append(list, host)
				}
			}
			if len(list) == 0 {
				continue
			}
			// Key by array element, so two elements of one array cannot merge.
			// Also key by the bare job id when the job has a single task, which is
			// how a job that is not an array appears; that bare key is the one a
			// non-array row looks up, and an array row never asks for it.
			hosts[JobKey(id, strings.TrimSpace(task.TaskNumber))] = list
			if len(job.Tasks) == 1 {
				hosts[id] = list
			}
		}
	}
	return hosts, nil
}

// xmlRootElement returns the name of a document's root element.
func xmlRootElement(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return "", fmt.Errorf("no XML element found")
		}
		if err != nil {
			return "", err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// hostFromQueueInstance takes the host out of "queue@host", returning "" when
// the value names no queue instance.
func hostFromQueueInstance(queue string) string {
	if at := strings.IndexByte(queue, '@'); at >= 0 {
		return queue[at+1:]
	}
	return ""
}
