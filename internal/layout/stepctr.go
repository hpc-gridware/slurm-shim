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

// PeekStep returns the id NextStep would issue, without consuming it. It exists
// for the dry run (SLURM_SHIM_DRY_RUN), which must report the step a real srun
// would create while leaving the counter untouched.
//
// It reports the same failures NextStep does rather than substituting 0: a
// corrupt counter makes the real srun exit 1, and a dry run that answered
// "step id 0" for that job would report success for a step that cannot be
// created. A missing counter is not an error -- that is the state a freshly
// initialized job is in, and the first id is 0.
//
// No lock is taken (the read cannot corrupt the counter), so a concurrent
// NextStep on the master host can make the returned id stale. That is acceptable
// for a report and is why this must not be used to reserve anything.
func PeekStep(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil
	}
	last, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("corrupt step counter %s: %q", path, s)
	}
	return last + 1, nil
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
