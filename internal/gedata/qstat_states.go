package gedata

import (
	"encoding/xml"
	"strings"
)

// JobRow is one job (or array task) parsed from qstat output.
type JobRow struct {
	JobID  string
	Name   string
	User   string
	State  string // raw GE state code, e.g. "r", "qw", "Eqw"
	Queue  string // queue instance, e.g. "gpu.q@node001"
	Slots  int
	TaskID string // array task id, "" when not an array
}

// MapState maps a GE state code to a SLURM compact state (REQ-SQU-001, SI-05).
// Order matters: deletion and error prefixes take precedence over the run/queue
// letters they may accompany.
func MapState(ge string) string {
	ge = strings.TrimSpace(ge)
	switch {
	case ge == "":
		return "CD" // absent from qstat means completed/gone
	case strings.ContainsRune(ge, 'd'):
		return "CG" // deletion in progress (dr, dt, dRr, ...)
	case strings.ContainsRune(ge, 'E'):
		return "PD" // error state, still queued (Eqw, Ehqw)
	case strings.ContainsAny(ge, "sST"):
		return "S" // suspended (s, ts, S, tS, T, tT)
	case strings.ContainsAny(ge, "rt"):
		return "R" // running or transferring (r, Rr, t, Rt)
	default:
		return "PD" // waiting/held/queued (qw, hqw, w, Rq, hRwq)
	}
}

// FullState maps a SLURM compact state to its long form (squeue %T).
func FullState(compact string) string {
	switch compact {
	case "PD":
		return "PENDING"
	case "R":
		return "RUNNING"
	case "S":
		return "SUSPENDED"
	case "CG":
		return "COMPLETING"
	case "CD":
		return "COMPLETED"
	case "F":
		return "FAILED"
	default:
		return compact
	}
}

// qstat -xml structure. Running jobs appear under queue_info, pending jobs under
// a nested job_info; both hold job_list elements.
type qstatXML struct {
	XMLName    xml.Name     `xml:"job_info"`
	QueueJobs  []xmlJobList `xml:"queue_info>job_list"`
	QueuedJobs []xmlJobList `xml:"job_info>job_list"`
}

type xmlJobList struct {
	Number string `xml:"JB_job_number"`
	Name   string `xml:"JB_name"`
	Owner  string `xml:"JB_owner"`
	State  string `xml:"state"`
	Queue  string `xml:"queue_name"`
	Slots  int    `xml:"slots"`
	Tasks  string `xml:"tasks"`
}

// ParseQstatXML parses `qstat -xml` output into job rows.
func ParseQstatXML(data []byte) ([]JobRow, error) {
	var doc qstatXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var rows []JobRow
	add := func(j xmlJobList) {
		rows = append(rows, JobRow{
			JobID:  j.Number,
			Name:   j.Name,
			User:   j.Owner,
			State:  j.State,
			Queue:  j.Queue,
			Slots:  j.Slots,
			TaskID: strings.TrimSpace(j.Tasks),
		})
	}
	for _, j := range doc.QueueJobs {
		add(j)
	}
	for _, j := range doc.QueuedJobs {
		add(j)
	}
	return rows, nil
}
