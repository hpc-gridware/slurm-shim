// Package mux handles srun-side output: expanding %-patterns into per-rank file
// paths and demultiplexing framed rank output onto srun's stdout/stderr with
// -l labeling (spec sec. 7.4).
package mux

import (
	"strconv"
	"strings"
)

// PatternFields are the values substituted into an output pattern.
type PatternFields struct {
	JobID    int64
	StepID   int
	Rank     int
	NodeID   int
	NodeName string
}

// ExpandPattern substitutes SLURM output-pattern verbs (REQ-RUN-003):
//
//	%j job id, %J job.step, %t rank, %n node id, %N node name, %s step id, %% literal %.
//
// An unknown %x is left verbatim (leading % preserved).
func ExpandPattern(pattern string, f PatternFields) string {
	var b strings.Builder
	b.Grow(len(pattern))
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 >= len(pattern) {
			b.WriteByte(pattern[i])
			continue
		}
		i++
		switch pattern[i] {
		case 'j':
			b.WriteString(strconv.FormatInt(f.JobID, 10))
		case 'J':
			b.WriteString(strconv.FormatInt(f.JobID, 10))
			b.WriteByte('.')
			b.WriteString(strconv.Itoa(f.StepID))
		case 't':
			b.WriteString(strconv.Itoa(f.Rank))
		case 'n':
			b.WriteString(strconv.Itoa(f.NodeID))
		case 'N':
			b.WriteString(f.NodeName)
		case 's':
			b.WriteString(strconv.Itoa(f.StepID))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}
