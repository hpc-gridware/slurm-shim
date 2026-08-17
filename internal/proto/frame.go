// Package proto defines the wire protocol between srun and the per-host stepper:
// the binary frame codec (D-2), the argv Envelope and channel StepSpec split
// (SI-35), and the authenticated control channel (D-1, sec. 7.11). Frames are
// length-prefixed and byte-transparent, so arbitrary rank output survives the
// mux/demux round trip unchanged.
package proto

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// FrameType identifies a control-channel message.
type FrameType uint8

const (
	// FrameHello is stepper->srun: the first frame, carrying the auth token.
	FrameHello FrameType = iota + 1
	// FrameSpec is srun->stepper: the StepSpec JSON (env, ranks) after auth.
	FrameSpec
	// FrameReady is stepper->srun: the stepper has decoded the spec and is
	// about to spawn ranks.
	FrameReady
	// FrameOut is stepper->srun: one line or chunk of rank output.
	FrameOut
	// FrameRankExit is stepper->srun: a rank exited; payload is a big-endian
	// int32 exit code.
	FrameRankExit
	// FrameRankFail is stepper->srun: a rank could not be started (pre-exec
	// failure); payload is the reason.
	FrameRankFail
	// FrameSig is srun->stepper: forward this signal to every rank; payload is
	// a big-endian int32 signal number.
	FrameSig
	// FramePing / FramePong carry liveness in both directions.
	FramePing
	FramePong
)

// Frame flag bits (used by FrameOut).
const (
	// FlagStderr marks output from the rank's stderr rather than stdout.
	FlagStderr uint8 = 1 << 0
	// FlagEOL marks a chunk that ended with a newline (the demux appends the
	// newline and, for -l, emits the label on the next chunk).
	FlagEOL uint8 = 1 << 1
)

// MaxPayload bounds a single frame's payload so a garbage length prefix cannot
// force a huge allocation (D-2 caps output chunks at 64 KiB; StepSpec and other
// control payloads are small, so this margin is generous).
const MaxPayload = 1 << 20

// MaxChunk is the output-framing chunk size: the stepper splits a rank's output
// into lines, or 64 KiB chunks for a line longer than this (D-2).
const MaxChunk = 64 * 1024

// headerLen is the fixed frame header: type(1) + flags(1) + rank(4) + len(4).
const headerLen = 10

// Frame is one decoded control-channel message.
type Frame struct {
	Type    FrameType
	Rank    uint32
	Flags   uint8
	Payload []byte
}

// FrameWriter serializes frames to a stream. Its Write is safe for concurrent
// use: each frame is assembled and written under one lock acquisition so frames
// from different rank readers never interleave (writes above PIPE_BUF are not
// atomic otherwise).
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter wraps w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write encodes and writes one frame.
func (fw *FrameWriter) Write(f Frame) error {
	if len(f.Payload) > MaxPayload {
		return fmt.Errorf("proto: payload %d exceeds max %d", len(f.Payload), MaxPayload)
	}
	buf := make([]byte, headerLen+len(f.Payload))
	buf[0] = byte(f.Type)
	buf[1] = f.Flags
	binary.BigEndian.PutUint32(buf[2:6], f.Rank)
	binary.BigEndian.PutUint32(buf[6:10], uint32(len(f.Payload)))
	copy(buf[headerLen:], f.Payload)

	fw.mu.Lock()
	defer fw.mu.Unlock()
	_, err := fw.w.Write(buf)
	return err
}

// FrameReader decodes frames from a stream.
type FrameReader struct {
	r *bufio.Reader
}

// NewFrameReader wraps r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReader(r)}
}

// Read decodes the next frame, or returns io.EOF at a clean end of stream.
func (fr *FrameReader) Read() (Frame, error) {
	var header [headerLen]byte
	if _, err := io.ReadFull(fr.r, header[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(header[6:10])
	if n > MaxPayload {
		return Frame{}, fmt.Errorf("proto: payload length %d exceeds max %d", n, MaxPayload)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{
		Type:    FrameType(header[0]),
		Flags:   header[1],
		Rank:    binary.BigEndian.Uint32(header[2:6]),
		Payload: payload,
	}, nil
}

// EncodeInt32 / DecodeInt32 carry exit codes and signal numbers in payloads.
func EncodeInt32(v int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(v))
	return b
}

// DecodeInt32 reads a 4-byte big-endian payload; it returns 0 for a short one.
func DecodeInt32(b []byte) int32 {
	if len(b) < 4 {
		return 0
	}
	return int32(binary.BigEndian.Uint32(b))
}
