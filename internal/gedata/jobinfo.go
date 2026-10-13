package gedata

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// JobInfo is what `qstat -xml -j` adds to the plain qstat listing for one
// job: the fields squeue renders beyond id, name, user and state, and the facts
// a pending job's reason is derived from. Every field is the job's request or
// its scheduler bookkeeping, so it is the same for all tasks of an array except
// the holds, which are per task (see Hold).
type JobInfo struct {
	Account string
	// WorkDir is where the job runs: its -cwd/-wd directory, else the owner's
	// home directory, which is where Grid Engine starts a job without one.
	WorkDir string
	PE      string
	// Queues are the cluster queues the job requested hard (-q), "@host" removed.
	Queues []string
	Submit time.Time
	// Begin is the qsub -a start time; zero when the job has none.
	Begin time.Time
	// Requests are the job's hard resource requests (-l) by complex name.
	Requests map[string]ResourceRequest
	// Predecessors are the jobs this one still waits for (-hold_jid,
	// -hold_jid_ad). Grid Engine drops a predecessor from the list once it has
	// finished, so a non-empty list means the dependency is unresolved.
	Predecessors []string
	// SchedulerMessages are the scheduler's reasons for not dispatching the job;
	// empty unless the scheduler config has schedd_job_info enabled.
	SchedulerMessages []string

	userHold, adminHold, arrayHold []taskRange
	taskHold                       map[int]uint
}

// ResourceRequest is one hard resource request: the value as written and its
// parsed amount (bytes for a MEMORY complex, seconds for a TIME complex).
type ResourceRequest struct {
	Value  string
	Amount float64
}

// Hold kinds reported by JobInfo.Hold.
const (
	HoldNone       = ""
	HoldUser       = "user"       // qhold / qsub -h
	HoldAdmin      = "admin"      // operator or system hold
	HoldDependency = "dependency" // array task dependency (-hold_jid_ad)
)

// Hold bits of JAT_hold for a task that is already enrolled (a rescheduled or
// partly run array), matching Grid Engine's MINUS_H_TGT_* values.
const (
	holdBitUser     = 1
	holdBitOperator = 2
	holdBitSystem   = 4
	holdBitArray    = 8
)

// Hold reports which hold keeps GE task geTask from running. geTask is a task
// number, the "lo-hi:step" of a pending range (its first task is asked about:
// qstat groups only tasks in the same state into one range), or "" for a job
// that is not an array, which is task 1.
func (d JobInfo) Hold(geTask string) string {
	task := firstTask(geTask)
	if bits, ok := d.taskHold[task]; ok {
		switch {
		case bits&holdBitUser != 0:
			return HoldUser
		case bits&(holdBitOperator|holdBitSystem) != 0:
			return HoldAdmin
		case bits&holdBitArray != 0:
			return HoldDependency
		}
		return HoldNone
	}
	switch {
	case inRanges(d.userHold, task):
		return HoldUser
	case inRanges(d.adminHold, task):
		return HoldAdmin
	case inRanges(d.arrayHold, task):
		return HoldDependency
	}
	return HoldNone
}

// JobInfoSet is one `qstat -xml -j` answer: each job's detail by job id, the
// granted hosts (the same document JobAllocations reads, so a listing that
// needs both costs one query), and whether the scheduler records why jobs wait.
type JobInfoSet struct {
	Jobs        map[string]JobInfo
	Allocations map[string]JobAllocation
	// SchedulerInfo is false when qstat says the collection of scheduler job
	// information is turned off (schedd_job_info false, the default), so the
	// absence of a scheduler message says nothing about a job.
	SchedulerInfo bool
}

// JobInfos runs ONE `qstat -xml -j` for the given job ids (comma-joined, the
// list form qstat accepts; ids it no longer knows are skipped), or for every
// job when ids is empty. Array ids ("4711_2") are reduced to their job.
func JobInfos(ctx context.Context, r Runner, ids []string) (JobInfoSet, error) {
	selector := "*"
	if len(ids) > 0 {
		var jobs []string
		for _, id := range ids {
			if b := baseJobID(id); !contains(jobs, b) {
				jobs = append(jobs, b)
			}
		}
		selector = strings.Join(jobs, ",")
	}
	out, errOut, exit, err := r.Run(ctx, "qstat", "-xml", "-j", selector)
	if err != nil {
		return JobInfoSet{}, fmt.Errorf("qstat -xml -j: %w", err)
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = "qstat reported no detail"
		}
		return JobInfoSet{}, fmt.Errorf("qstat -xml -j: exit %d: %s", exit, msg)
	}
	return ParseJobInfoXML(out)
}

// jobInfoXML reads the subset of `qstat -xml -j` JobInfo needs. The
// go-clusterscheduler qstat parsers read the plain-text `qstat -j` view, which
// has no field for the -a start time and keeps only the requested (not the
// still-pending) predecessors; the XML view carries both, and is the view the
// host lists already come from. Lists whose entry element name differs between
// releases are read with ",any".
type jobInfoXML struct {
	Jobs []struct {
		JobID       string        `xml:"JB_job_number"`
		Account     string        `xml:"JB_account"`
		Cwd         string        `xml:"JB_cwd"`
		PE          string        `xml:"JB_pe"`
		Submit      string        `xml:"JB_submission_time"`
		Execution   string        `xml:"JB_execution_time"`
		Env         []variableXML `xml:"JB_env_list>job_sublist"`
		HardQueues  []string      `xml:"JB_hard_queue_list>destin_ident_list>QR_name"`
		HardRes     []requestXML  `xml:"JB_hard_resource_list>qstat_l_requests"`
		RequestSets []struct {
			HardQueues []string     `xml:"JRS_hard_queue_list>destin_ident_list>QR_name"`
			HardRes    []requestXML `xml:"JRS_hard_resource_list>qstat_l_requests"`
		} `xml:"JB_request_set_list>ulong_sublist"`
		Predecessors      jobRefsXML    `xml:"JB_jid_predecessor_list"`
		ArrayPredecessors jobRefsXML    `xml:"JB_ja_ad_predecessor_list"`
		UserHold          []taskRange   `xml:"JB_ja_u_h_ids>task_id_range"`
		OperatorHold      []taskRange   `xml:"JB_ja_o_h_ids>task_id_range"`
		SystemHold        []taskRange   `xml:"JB_ja_s_h_ids>task_id_range"`
		ArrayHold         []taskRange   `xml:"JB_ja_a_h_ids>task_id_range"`
		Tasks             []taskHoldXML `xml:"JB_ja_tasks>element"`
	} `xml:"djob_info>element"`
	Messages []struct {
		PerJob []struct {
			Jobs struct {
				Items []struct {
					Value string `xml:"ULNG_value"`
				} `xml:",any"`
			} `xml:"MES_job_number_list"`
			Text string `xml:"MES_message"`
		} `xml:"SME_message_list>element"`
		Global []string `xml:"SME_global_message_list>element>MES_message"`
	} `xml:"messages>element"`
}

type requestXML struct {
	Name      string  `xml:"CE_name"`
	StringVal string  `xml:"CE_stringval"`
	DoubleVal float64 `xml:"CE_doubleval"`
}

type jobRefsXML struct {
	Items []struct {
		JobNumber string `xml:"JRE_job_number"`
	} `xml:",any"`
}

type taskHoldXML struct {
	Number string `xml:"JAT_task_number"`
	Hold   uint   `xml:"JAT_hold"`
}

// taskRange is a Grid Engine task id range (RN_min, RN_max, RN_step).
type taskRange struct {
	Min  int `xml:"RN_min"`
	Max  int `xml:"RN_max"`
	Step int `xml:"RN_step"`
}

// schedulerInfoOff is the text of qstat's global message when schedd_job_info
// is false.
const schedulerInfoOff = "scheduler job information is turned off"

// ParseJobInfoXML parses a `qstat -xml -j` document into a JobInfoSet.
func ParseJobInfoXML(data []byte) (JobInfoSet, error) {
	set := JobInfoSet{Jobs: map[string]JobInfo{}, Allocations: map[string]JobAllocation{}, SchedulerInfo: true}
	root, err := xmlRootElement(data)
	if err != nil {
		return set, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	if root == "unknown_jobs" {
		return set, nil // no job matched: see ParseJobAllocationsXML
	}
	var doc jobInfoXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return set, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	if set.Allocations, err = ParseJobAllocationsXML(data); err != nil {
		return set, err
	}
	for _, j := range doc.Jobs {
		id := strings.TrimSpace(j.JobID)
		if id == "" {
			continue
		}
		d := JobInfo{
			Account:   strings.TrimSpace(j.Account),
			WorkDir:   strings.TrimSpace(j.Cwd),
			PE:        strings.TrimSpace(j.PE),
			Submit:    qstatTime(j.Submit),
			Begin:     qstatTime(j.Execution), // "0" when unset parses to zero
			Requests:  map[string]ResourceRequest{},
			userHold:  j.UserHold,
			adminHold: append(append([]taskRange(nil), j.OperatorHold...), j.SystemHold...),
			arrayHold: j.ArrayHold,
		}
		if d.WorkDir == "" {
			d.WorkDir = envValue(j.Env, "__SGE_PREFIX__O_HOME")
		}
		queues := j.HardQueues
		addRequests(d.Requests, j.HardRes)
		for _, rs := range j.RequestSets {
			queues = append(queues, rs.HardQueues...)
			addRequests(d.Requests, rs.HardRes)
		}
		for _, q := range queues {
			q, _, _ = strings.Cut(strings.TrimSpace(q), "@")
			if q != "" && !contains(d.Queues, q) {
				d.Queues = append(d.Queues, q)
			}
		}
		for _, p := range append(j.Predecessors.Items, j.ArrayPredecessors.Items...) {
			if n := strings.TrimSpace(p.JobNumber); n != "" && n != "0" && !contains(d.Predecessors, n) {
				d.Predecessors = append(d.Predecessors, n)
			}
		}
		for _, t := range j.Tasks {
			if n, err := strconv.Atoi(strings.TrimSpace(t.Number)); err == nil {
				if d.taskHold == nil {
					d.taskHold = map[int]uint{}
				}
				d.taskHold[n] = t.Hold
			}
		}
		set.Jobs[id] = d
	}
	for _, m := range doc.Messages {
		for _, g := range m.Global {
			if strings.Contains(g, schedulerInfoOff) {
				set.SchedulerInfo = false
			}
		}
		for _, pj := range m.PerJob {
			for _, item := range pj.Jobs.Items {
				id := strings.TrimSpace(item.Value)
				if d, ok := set.Jobs[id]; ok {
					d.SchedulerMessages = append(d.SchedulerMessages, strings.TrimSpace(pj.Text))
					set.Jobs[id] = d
				}
			}
		}
	}
	return set, nil
}

// addRequests records each request once: the global request set comes first in
// the document, so a per-host (master/slave) scope never overrides it.
func addRequests(into map[string]ResourceRequest, reqs []requestXML) {
	for _, r := range reqs {
		name := strings.TrimSpace(r.Name)
		if _, seen := into[name]; name == "" || seen {
			continue
		}
		into[name] = ResourceRequest{Value: strings.TrimSpace(r.StringVal), Amount: r.DoubleVal}
	}
}

func envValue(env []variableXML, name string) string {
	for _, v := range env {
		if v.Name == name {
			return strings.TrimSpace(v.Value)
		}
	}
	return ""
}

func inRanges(ranges []taskRange, task int) bool {
	for _, r := range ranges {
		step := r.Step
		if step < 1 {
			step = 1
		}
		if task >= r.Min && task <= r.Max && (task-r.Min)%step == 0 {
			return true
		}
	}
	return false
}
