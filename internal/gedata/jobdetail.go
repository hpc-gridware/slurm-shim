package gedata

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// JobDetail is the part of one job's `qstat -xml -j` record that scontrol show
// job and scontrol update need. It is read from the XML view, not the plain
// `qstat -j` text: the plain view prints values verbatim, so a job submitted
// with -V and an exported shell function (BASH_FUNC_x%%=() { ...\n... }) spills
// continuation lines into the listing -- reproduced on OCS 9.1.6 -- and a line
// in an environment value could even pose as hard_resource_list. Elements in
// the XML view cannot be forged that way.
type JobDetail struct {
	JobID string
	Name  string
	Owner string
	UID   string
	Group string
	GID   string
	// Submit is the submission time; Begin is the -a start time (zero if none).
	Submit time.Time
	Begin  time.Time
	// Cwd is the -cwd/-wd directory, "" when the job runs in the owner's home.
	Cwd  string
	Home string // the submitter's $HOME (__SGE_PREFIX__O_HOME)
	// Stdout and Stderr are the first -o/-e path as submitted (pseudo variables
	// such as $JOB_ID unexpanded), "" when GE's default applies.
	Stdout      string
	Stderr      string
	MergeStderr bool
	Script      string
	Args        []string
	PE          string
	// Predecessors are the job ids of -hold_jid dependencies still pending.
	Predecessors []string
	// HardQueues is the global-scope -q request.
	HardQueues []string
	// HardResources is the global-scope hard -l list in qstat order. A scoped
	// (-scope master/slave) request is not part of it: qalter -l replaces the
	// global scope only (verified on OCS 9.1.6), so that is the list to merge.
	HardResources []Resource
	// UserHold holds the "lo-hi:step" ranges of tasks under a user hold.
	UserHold []string
	// Starts maps a GE task number to its start time, for started tasks.
	Starts map[string]time.Time
	// Allocations are the job's granted hosts, as JobAllocations returns them.
	Allocations map[string]JobAllocation
	// GPUs are the granted devices of the configured GPU complex.
	GPUs []HostGPUs
}

// Resource is one name=value hard resource request.
type Resource struct {
	Name  string
	Value string
}

// ResourceList renders requests the way qsub -l takes them: "a=1,b=2".
func ResourceList(res []Resource) string {
	parts := make([]string, len(res))
	for i, r := range res {
		parts[i] = r.Name + "=" + r.Value
	}
	return strings.Join(parts, ",")
}

// Resource returns the value of the named hard request.
func (d JobDetail) Resource(name string) (string, bool) {
	for _, r := range d.HardResources {
		if r.Name == name {
			return r.Value, true
		}
	}
	return "", false
}

// UserHeld reports whether GE task number task is under a user hold.
func (d JobDetail) UserHeld(task string) bool {
	for _, r := range d.UserHold {
		if TaskInRange(r, task) {
			return true
		}
	}
	return false
}

type jobDetailXML struct {
	Jobs []struct {
		Number      string        `xml:"JB_job_number"`
		Name        string        `xml:"JB_job_name"`
		Owner       string        `xml:"JB_owner"`
		UID         string        `xml:"JB_uid"`
		Group       string        `xml:"JB_group"`
		GID         string        `xml:"JB_gid"`
		Submit      string        `xml:"JB_submission_time"`
		Execution   string        `xml:"JB_execution_time"`
		Cwd         string        `xml:"JB_cwd"`
		Merge       string        `xml:"JB_merge_stderr"`
		Stdout      []string      `xml:"JB_stdout_path_list>path_list>PN_path"`
		Stderr      []string      `xml:"JB_stderr_path_list>path_list>PN_path"`
		Script      string        `xml:"JB_script_file"`
		Args        []string      `xml:"JB_job_args>element>ST_name"`
		PE          string        `xml:"JB_pe"`
		Predecessor []string      `xml:"JB_jid_predecessor_list>job_predecessors>JRE_job_number"`
		Env         []variableXML `xml:"JB_env_list>job_sublist"`
		Sets        []struct {
			Scope  string           `xml:"JRS_scope"`
			Hard   []hardRequestXML `xml:"JRS_hard_resource_list>qstat_l_requests"`
			Queues []string         `xml:"JRS_hard_queue_list>destin_ident_list>QR_name"`
		} `xml:"JB_request_set_list>ulong_sublist"`
		UserHold []rangeXML `xml:"JB_ja_u_h_ids>task_id_range"`
		Tasks    []struct {
			Number string `xml:"JAT_task_number"`
			Start  string `xml:"JAT_start_time"`
		} `xml:"JB_ja_tasks>element"`
	} `xml:"djob_info>element"`
}

// hardRequestXML is one hard -l request. CE_stringval is the value as GE
// holds it ("1G", "7200"), the form qalter -l takes back.
type hardRequestXML struct {
	Name      string `xml:"CE_name"`
	StringVal string `xml:"CE_stringval"`
}

type rangeXML struct {
	Min  int `xml:"RN_min"`
	Max  int `xml:"RN_max"`
	Step int `xml:"RN_step"`
}

// globalScope is JRS_scope for requests without -scope (or -scope global).
const globalScope = "0"

// FetchJobDetail runs `qstat -xml -j <job>` once and returns the job's detail,
// including its granted hosts and the GPUs of gpuComplex ("" for none). found
// is false when GE does not know the job.
func FetchJobDetail(ctx context.Context, r Runner, jobID, gpuComplex string) (detail JobDetail, found bool, err error) {
	id := baseJobID(jobID)
	out, errOut, exit, err := r.Run(ctx, "qstat", "-xml", "-j", id)
	if err != nil {
		return JobDetail{}, false, fmt.Errorf("qstat -xml -j: %w", err)
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = "qstat reported no detail"
		}
		return JobDetail{}, false, fmt.Errorf("qstat -xml -j: exit %d: %s", exit, msg)
	}
	detail, found, err = ParseJobDetailXML(out, id)
	if err != nil || !found {
		return detail, found, err
	}
	if detail.Allocations, err = ParseJobAllocationsXML(out); err != nil {
		return detail, true, err
	}
	if gpuComplex != "" {
		if detail.GPUs, err = ParseGrantedGPUsXML(out, gpuComplex); err != nil {
			return detail, true, err
		}
	}
	return detail, true, nil
}

// ParseJobDetailXML reads job jobID from `qstat -xml -j` output. found is false
// for qstat's <unknown_jobs> answer or when the document holds another job.
func ParseJobDetailXML(data []byte, jobID string) (detail JobDetail, found bool, err error) {
	root, err := xmlRootElement(data)
	if err != nil {
		return JobDetail{}, false, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	if root == "unknown_jobs" {
		return JobDetail{}, false, nil
	}
	var doc jobDetailXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return JobDetail{}, false, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	for _, j := range doc.Jobs {
		if strings.TrimSpace(j.Number) != jobID {
			continue
		}
		d := JobDetail{
			JobID:        jobID,
			Name:         j.Name,
			Owner:        j.Owner,
			UID:          j.UID,
			Group:        j.Group,
			GID:          j.GID,
			Submit:       qstatTime(j.Submit),
			Begin:        qstatTime(j.Execution), // "0" when no -a: zero time
			Cwd:          j.Cwd,
			MergeStderr:  strings.EqualFold(strings.TrimSpace(j.Merge), "true"),
			Script:       j.Script,
			Args:         j.Args,
			PE:           j.PE,
			Predecessors: j.Predecessor,
			Starts:       map[string]time.Time{},
		}
		if len(j.Stdout) > 0 {
			d.Stdout = j.Stdout[0]
		}
		if len(j.Stderr) > 0 {
			d.Stderr = j.Stderr[0]
		}
		for _, v := range j.Env {
			if v.Name == "__SGE_PREFIX__O_HOME" {
				d.Home = v.Value
			}
		}
		for _, set := range j.Sets {
			if strings.TrimSpace(set.Scope) != globalScope {
				continue
			}
			d.HardQueues = set.Queues
			for _, req := range set.Hard {
				d.HardResources = append(d.HardResources, Resource{Name: req.Name, Value: req.StringVal})
			}
		}
		for _, rg := range j.UserHold {
			d.UserHold = append(d.UserHold, fmt.Sprintf("%d-%d:%d", rg.Min, rg.Max, rg.Step))
		}
		for _, t := range j.Tasks {
			if start := qstatTime(t.Start); !start.IsZero() {
				d.Starts[strings.TrimSpace(t.Number)] = start
			}
		}
		return d, true, nil
	}
	return JobDetail{}, false, nil
}

// TaskStart returns when GE task number task started (zero when it has not).
// A job that is not an array runs as task 1.
func (d JobDetail) TaskStart(task string) time.Time {
	if task == "" {
		task = "1"
	}
	return d.Starts[task]
}
