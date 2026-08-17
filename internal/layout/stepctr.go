package layout

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// InitStepCounter creates the step counter file holding the last-issued step id
// as -1, so the first NextStep returns 0 (spec section 11.3). The fabricator
// calls this once at job start.
func InitStepCounter(path string) error {
	return os.WriteFile(path, []byte("-1\n"), 0o600)
}

// NextStep atomically issues the next step id under concurrent sruns from the
// master host: it takes an exclusive flock, reads the last id, increments,
// writes it back, and returns the new id (REQ-LCY-003). flock serializes even
// across separate file descriptions in the same process, so goroutine-driven
// callers are serialized too.
func NextStep(path string) (int, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	// Closing the descriptor releases the flock, so no explicit unlock is
	// needed. A close error on the counter file is not recoverable and does not
	// affect the already-issued id.
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}

	// ReadAt returns io.EOF when the file is shorter than the buffer, which is
	// the normal case for a small counter; only other errors are fatal.
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}

	last := -1
	if s := strings.TrimSpace(string(buf[:n])); s != "" {
		last, err = strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("corrupt step counter %s: %q", path, s)
		}
	}

	next := last + 1
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(next)+"\n"), 0); err != nil {
		return 0, err
	}
	return next, nil
}
