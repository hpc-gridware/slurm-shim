package gedata

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// JobAllocation is one job's granted hosts split into the hosts that run its
// tasks and its hot spares (sbatch --x-spares). Swaps recorded by srun (spare
// replacing a lost node) are already applied: a swapped-in spare is in Hosts and
// the host it replaced is in Lost.
type JobAllocation struct {
	Hosts  []string
	Spares []string
	Lost   []string
}

// SwapsContextKey is the job-context key under which srun records hot-spare swaps
// ("lost:spare" pairs joined by '+'; qalter -ac splits values on commas).
const SwapsContextKey = "shim.swaps"

// JobAllocations maps a job key (see JobKey) to the exec hosts holding that
// job's tasks, in the order the scheduler granted them, split into task hosts
// and hot spares (see JobAllocation): the spare count comes from the job's
// SLURM_SHIM_SPARES (set by sbatch), the swaps from its shim.swaps context.
// Pass a job id to ask about one job, or "" for every job the caller may see.
//
// The source is JAT_granted_destin_identifier_list in `qstat -xml -j`, which
// names each granted host in JG_qhostname, untruncated.
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
func JobAllocations(ctx context.Context, r Runner, jobID string) (map[string]JobAllocation, error) {
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
	hosts, err := ParseJobAllocationsXML(out)
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
		JobID   string        `xml:"JB_job_number"`
		Env     []variableXML `xml:"JB_env_list>job_sublist"`
		Context []variableXML `xml:"JB_context>context_list"`
		Tasks   []struct {
			TaskNumber string `xml:"JAT_task_number"`
			Granted    []struct {
				QHostname string `xml:"JG_qhostname"`
				QName     string `xml:"JG_qname"`
			} `xml:"JAT_granted_destin_identifier_list>element"`
		} `xml:"JB_ja_tasks>element"`
	} `xml:"djob_info>element"`
}

type variableXML struct {
	Name  string `xml:"VA_variable"`
	Value string `xml:"VA_value"`
}

// ParseJobAllocationsXML extracts the granted hosts per job from `qstat -xml -j`,
// split into task hosts and hot spares.
//
// Hosts keep the order the scheduler granted them and are never sorted: the
// nodelist encoding is first-seen order (REQ-ENC-002, as amended by SI-41),
// because a sorted encoding desynchronises a derived master address from rank-0
// placement.
func ParseJobAllocationsXML(data []byte) (map[string]JobAllocation, error) {
	// qstat answers a selector that matches nothing with an <unknown_jobs>
	// document whose entries are wrapped in a literal "<>" element. That is not
	// well-formed XML and no unmarshaller can read it, so recognise the root
	// before parsing: "no such job" is an ordinary answer, not a failure.
	root, err := xmlRootElement(data)
	if err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	if root == "unknown_jobs" {
		return map[string]JobAllocation{}, nil
	}

	var doc grantedJobsXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}

	hosts := map[string]JobAllocation{}
	for _, job := range doc.Jobs {
		id := strings.TrimSpace(job.JobID)
		if id == "" {
			continue
		}
		for _, task := range job.Tasks {
			var list []string
			for _, g := range task.Granted {
				// Reduce to the SHORT name. Both JG_qhostname and the queue
				// instance carry whatever form the cluster stores, and on a site
				// where Grid Engine registered its hosts by fully qualified name
				// that is the FQDN -- verified on a GCP cluster (2026-09-15), where
				// both fields read "shimval-g1.us-central1-b.c.<project>.internal".
				// The layout reduces every PE_HOSTFILE host the same way (see
				// splitHostname), so SLURM_JOB_NODELIST is short there. Reporting
				// the long form here would make squeue name hosts in a form the
				// job's own environment never uses, and a master address derived
				// from this column would not match rank 0's host.
				host := strings.TrimSpace(g.QHostname)
				if host == "" {
					// Older or partial output may carry only the queue instance.
					host = hostFromQueueInstance(g.QName)
				}
				host, _ = splitHostname(host)
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
			alloc := splitAllocation(list, job.Env, job.Context)
			hosts[JobKey(id, strings.TrimSpace(task.TaskNumber))] = alloc
			if len(job.Tasks) == 1 {
				hosts[id] = alloc
			}
		}
	}
	return hosts, nil
}

// splitAllocation takes the job's hot spares off the end of its grant -- the
// fabricator's rule: the last SLURM_SHIM_SPARES hosts -- and applies the swaps
// srun recorded, so a view from outside the job matches the layout inside it.
// A swap applies only when its lost host is one of the job's nodes and its spare
// still an unused spare of this grant: a requeued job may still carry the swaps of
// its previous run (ClearSwaps is best-effort), which name other hosts.
func splitAllocation(granted []string, env, context []variableXML) JobAllocation {
	k := 0
	for _, v := range env {
		if v.Name == "SLURM_SHIM_SPARES" {
			k, _ = strconv.Atoi(strings.TrimSpace(v.Value))
		}
	}
	if k <= 0 || k >= len(granted) {
		return JobAllocation{Hosts: granted}
	}
	n := len(granted) - k
	a := JobAllocation{
		Hosts:  append([]string(nil), granted[:n]...),
		Spares: append([]string(nil), granted[n:]...),
	}
	for _, v := range context {
		if v.Name != SwapsContextKey {
			continue
		}
		for _, pair := range strings.Split(v.Value, "+") {
			lost, spare, _ := strings.Cut(pair, ":")
			i, j := slices.Index(a.Hosts, lost), slices.Index(a.Spares, spare)
			if i < 0 || j < 0 {
				continue
			}
			a.Hosts[i] = spare
			a.Lost = append(a.Lost, lost)
			a.Spares = append(a.Spares[:j], a.Spares[j+1:]...)
		}
	}
	return a
}

// RecordSwaps stores the job's hot-spare swaps ("lost:spare" pairs, in order) in
// its job context, so squeue and scontrol outside the job can show them. qalter
// -ac on a running job works only from a submit host; the caller treats failure
// as best-effort.
func RecordSwaps(ctx context.Context, r Runner, jobID string, pairs []string) error {
	return runQalter(ctx, r, "-ac", SwapsContextKey+"="+strings.Join(pairs, "+"), jobID)
}

// ClearSwaps removes recorded swaps, so a requeued job does not show the swaps
// of its previous run.
func ClearSwaps(ctx context.Context, r Runner, jobID string) error {
	return runQalter(ctx, r, "-dc", SwapsContextKey, jobID)
}

func runQalter(ctx context.Context, r Runner, args ...string) error {
	_, errOut, exit, err := r.Run(ctx, "qalter", args...)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("qalter: exit %d: %s", exit, strings.TrimSpace(string(errOut)))
	}
	return nil
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
