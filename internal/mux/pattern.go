// Package mux handles srun-side output: expanding %-patterns into per-rank file
// paths and demultiplexing framed rank output onto srun's stdout/stderr with
// -l labeling (spec sec. 7.4).
package mux

import (
	"strconv"
	"strings"
)

// PatternFields are the values substituted into an output pattern. ArrayJobID
// and ArrayTaskID carry the SLURM array coordinates (0-based, as submitit reads
// them); for a non-array job they default to the plain job id and 0.
type PatternFields struct {
	JobID       int64
	ArrayJobID  int64
	ArrayTaskID int64
	StepID      int
	Rank        int
	NodeID      int
	NodeName    string
	JobName     string
	User        string
}

// ExpandPattern substitutes SLURM output-pattern verbs (REQ-RUN-003):
//
//	%j job id, %J job.step, %A array job id, %a array task id (0-based),
//	%t rank, %n node id, %N node name, %s step id, %x job name, %u user,
//	%% literal %.
//
// An unknown verb keeps its leading % and letter; a zero-pad width (%3a) is
// dropped, since the expansion cannot pad.
func ExpandPattern(pattern string, f PatternFields) string {
	var b strings.Builder
	b.Grow(len(pattern))
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 >= len(pattern) {
			b.WriteByte(pattern[i])
			continue
		}
		i++
		// SLURM allows a zero-pad width (%3a); we cannot pad, but the verb must
		// still expand or every rank writes to one literal filename.
		for i < len(pattern) && pattern[i] >= '0' && pattern[i] <= '9' {
			i++
		}
		if i >= len(pattern) {
			b.WriteByte('%')
			break
		}
		switch pattern[i] {
		case 'j':
			b.WriteString(strconv.FormatInt(f.JobID, 10))
		case 'J':
			b.WriteString(strconv.FormatInt(f.JobID, 10))
			b.WriteByte('.')
			b.WriteString(strconv.Itoa(f.StepID))
		case 'A':
			b.WriteString(strconv.FormatInt(f.ArrayJobID, 10))
		case 'a':
			b.WriteString(strconv.FormatInt(f.ArrayTaskID, 10))
		case 't':
			b.WriteString(strconv.Itoa(f.Rank))
		case 'n':
			b.WriteString(strconv.Itoa(f.NodeID))
		case 'N':
			b.WriteString(f.NodeName)
		case 's':
			b.WriteString(strconv.Itoa(f.StepID))
		case 'x':
			b.WriteString(f.JobName)
		case 'u':
			b.WriteString(f.User)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}
