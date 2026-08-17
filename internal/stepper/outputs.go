package stepper

import (
	"fmt"
	"os"
)

// outputFiles holds a rank's pattern output files, or nil streams when output
// is framed back over the channel instead.
type outputFiles struct {
	stdout *os.File
	stderr *os.File
}

// openOutputs opens every rank's pattern output file before any rank is spawned
// (SI-21): a bad path fails the whole host atomically rather than after some
// ranks have already started. On any failure it closes what it opened.
func (s *stepper) openOutputs() ([]*outputFiles, error) {
	files := make([]*outputFiles, len(s.spec.Ranks))
	for i := range s.spec.Ranks {
		files[i] = &outputFiles{}
	}
	created := []*os.File{}
	fail := func(err error) ([]*outputFiles, error) {
		closeAll2(created)
		return nil, err
	}
	for i, r := range s.spec.Ranks {
		if r.StdoutFile != "" {
			f, err := os.Create(r.StdoutFile)
			if err != nil {
				return fail(fmt.Errorf("opening %s: %w", r.StdoutFile, err))
			}
			files[i].stdout = f
			created = append(created, f)
		}
		if r.StderrFile != "" {
			f, err := os.Create(r.StderrFile)
			if err != nil {
				return fail(fmt.Errorf("opening %s: %w", r.StderrFile, err))
			}
			files[i].stderr = f
			created = append(created, f)
		}
	}
	return files, nil
}

func closeAll(files []*outputFiles) {
	for _, f := range files {
		if f == nil {
			continue
		}
		if f.stdout != nil {
			_ = f.stdout.Close()
		}
		if f.stderr != nil {
			_ = f.stderr.Close()
		}
	}
}

func closeAll2(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
