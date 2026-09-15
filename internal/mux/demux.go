package mux

import (
	"io"
	"strconv"
	"sync"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// Demux writes framed rank output onto srun's stdout/stderr. It applies -l
// labeling at line starts and reassembles long lines split across chunks using
// the frame EOL flag, so output is byte-faithful with no cross-rank tearing
// (REQ-RUN-020). It is safe for concurrent Handle calls from multiple stepper
// readers.
type Demux struct {
	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer
	label  bool
	// midline tracks, per (rank, stream), whether the last chunk left the line
	// open (no EOL yet), so the next chunk suppresses the label and Flush can
	// terminate a dangling partial line.
	midline map[key]bool
}

type key struct {
	rank   uint32
	stderr bool
}

// NewDemux writes stream output to stdout and stderr, optionally prefixing each
// line with "<rank>: " (srun -l).
func NewDemux(stdout, stderr io.Writer, label bool) *Demux {
	return &Demux{stdout: stdout, stderr: stderr, label: label, midline: map[key]bool{}}
}

// Handle processes one FrameOut. Non-output frames are ignored.
func (d *Demux) Handle(f proto.Frame) error {
	if f.Type != proto.FrameOut {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	isErr := f.Flags&proto.FlagStderr != 0
	k := key{rank: f.Rank, stderr: isErr}
	w := d.stdout
	if isErr {
		w = d.stderr
	}

	// ONE Write per frame: label, payload and newline are assembled first and
	// issued as a single call. The mutex above only orders writes within THIS
	// process, and a job script may background several srun steps that all
	// inherited the same stdout -- which is exactly what Ray's head-plus-workers
	// recipe does. Emitting a line in two or three writes lets another srun slip
	// its own line in between the payload and the newline, so two ranks' lines end
	// up concatenated on one line. That was observed on a four-node GCP cluster
	// (2026-09-15): a worker on one host and a worker on another shared a line,
	// and anything counting lines saw three steps as two. Concurrent sruns share
	// one file description, so a single write per line is what keeps them apart.
	line := make([]byte, 0, len(f.Payload)+16)
	if d.label && !d.midline[k] {
		line = append(line, strconv.FormatUint(uint64(f.Rank), 10)...)
		line = append(line, ':', ' ')
	}
	line = append(line, f.Payload...)

	if f.Flags&proto.FlagEOL != 0 {
		line = append(line, '\n')
		d.midline[k] = false
	} else {
		d.midline[k] = true
	}
	if _, err := w.Write(line); err != nil {
		return err
	}
	return nil
}

// Flush terminates any dangling partial lines with a trailing newline
// (REQ-RUN-020). Call it once after all steppers have finished.
func (d *Demux) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, open := range d.midline {
		if !open {
			continue
		}
		w := d.stdout
		if k.stderr {
			w = d.stderr
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		d.midline[k] = false
	}
	return nil
}
