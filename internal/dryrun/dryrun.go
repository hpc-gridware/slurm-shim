// Package dryrun implements the SLURM_SHIM_DRY_RUN mode: mutating commands
// report what they would do and change nothing. Read-only Grid Engine clients
// (qstat, qconf, qacct, qhost) still run, so a dry run can resolve real cluster
// state -- the PE's allocation rule, a partition's queue -- instead of guessing.
//
// Two mechanisms, deliberately:
//
//   - sbatch and srun BRANCH on Enabled() to render their report, because there is
//     no honest fake job id, no wrapper to spool and no step id to consume.
//   - every command additionally WRAPS its Runner, so a mutating GE client cannot
//     be reached by a code path that forgot the branch. The wrapper covers only
//     clients invoked through gedata.Runner; srun's qrsh launch bypasses that
//     interface entirely (internal/launch), so it is guarded by its branch alone.
//
// Reports go to stderr. Real SLURM does the same for `sbatch --test-only`, and
// this shim's REQ-LOG-003 reserves stdout for command output -- for srun, stdout
// is the ranks' own output stream.
package dryrun

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// EnvVar names the environment variable that turns dry-run mode on.
const EnvVar = "SLURM_SHIM_DRY_RUN"

// ExitFatal is the exit code a dry run returns when the report itself proves the
// request cannot run (an unsatisfiable task policy, an undispatchable slot count,
// an unusable launcher). Reserving 0 for "this would run" is what makes
// `sbatch --test-only job.sh && sbatch job.sh` a usable gate.
const ExitFatal = 1

// Enabled reports whether dry-run mode is on for this process.
func Enabled() bool { return On(os.Getenv(EnvVar)) }

// On reports whether a raw SLURM_SHIM_DRY_RUN value enables dry-run mode.
//
// The ON spellings are allowlisted rather than the OFF ones: this mode's ON state
// SUPPRESSES work, so an unrecognized value must fail open to "do the real thing".
// The reverse polarity made SLURM_SHIM_DRY_RUN=n -- the likeliest way a person
// spells "no" -- silently turn every job into a no-op.
//
// Unrecognized non-empty values are reported by Unrecognized so the caller can
// warn rather than silently ignoring what the user meant.
func On(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// Unrecognized returns the raw SLURM_SHIM_DRY_RUN value when it is neither a
// recognized on- nor off-spelling, so commands can warn that it was ignored.
// Empty when the value is absent or understood.
func Unrecognized() string { return unrecognized(os.Getenv(EnvVar)) }

func unrecognized(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "n", "off", "1", "true", "yes", "y", "on":
		return ""
	}
	return v
}

// mutating lists the Grid Engine clients that change cluster state and are
// reachable through gedata.Runner. The shim invokes exactly these three today;
// the list is the intercept set, not an inventory of GE, so a client is added
// here when the shim learns to call it.
//
// qrsh is deliberately absent: it mutates job state, but internal/launch execs it
// directly rather than through Runner, so it cannot be intercepted here. srun's
// Enabled() branch is what guards it.
var mutating = map[string]bool{
	"qsub": true,
	"qdel": true,
	"qmod": true,
}

// Runner wraps a gedata.Runner so mutating GE clients are reported instead of
// executed.
//
// It is a backstop, not the primary mechanism: for scancel and scontrol requeue
// it IS the implementation, but for sbatch and srun the explicit branch renders
// the report and this only guarantees that a forgotten branch degrades to a
// reported no-op rather than a live submission.
type Runner struct {
	// Inner executes the read-only clients.
	Inner gedata.Runner
	// Out receives the "would run" lines (stderr).
	Out io.Writer
	// Prefix is the user-facing command name for diagnostics ("scancel").
	Prefix string
}

// Run reports and skips a mutating client, and delegates everything else.
func (r Runner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	if !mutating[name] || printsUsageOnly(name, args) {
		return r.Inner.Run(ctx, name, args...)
	}
	fmt.Fprintf(r.Out, "%s: dry run: would run: %s\n", r.Prefix, Command(name, args))
	return nil, nil, 0, nil
}

// printsUsageOnly reports whether a mutating client was invoked in a form that
// only prints its usage text and cannot touch cluster state.
//
// `qsub -help` is the capability probe (does this client understand -par?).
// Intercepting it would answer "no" under a dry run and "yes" on the real submit
// path, so the report would contradict the submission it is supposed to explain --
// and it would blame the cluster's OCS version for a mode the user turned on.
func printsUsageOnly(name string, args []string) bool {
	return name == "qsub" && len(args) == 1 && args[0] == "-help"
}

// Wrap returns r wrapped for dry-run mode, or r unchanged when the mode is off.
// out must be stderr: the reported line is a diagnostic, not command output.
func Wrap(r gedata.Runner, out io.Writer, prefix string) gedata.Runner {
	if !Enabled() {
		return r
	}
	return Runner{Inner: r, Out: out, Prefix: prefix}
}

// Banner is the leading line every dry run prints, so the reason nothing happened
// is never in doubt even when the command produced no other output.
func Banner(cmd string) string {
	return cmd + ": dry run (" + EnvVar + " is set) -- no job was submitted or launched"
}

// Command renders a command line for display. Every token including the command
// name is quoted when it holds anything a shell would treat specially, so the
// rendering cannot imply a command the shim would not run.
//
// The result is safe to READ. It is not promised to be pasteable: a --wrap or
// wrapper-mode submission names a temp path that only exists at submit time, and
// secret values are redacted by the caller.
func Command(name string, args []string) string {
	var b strings.Builder
	b.WriteString(Quote(name))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(Quote(a))
	}
	return b.String()
}

// Quote single-quotes s when it holds anything outside the shell-safe set,
// escaping embedded quotes the POSIX way. An empty string becomes an empty quoted
// pair, so it stays visible in a reported command line.
//
// Control characters are escaped rather than passed through: report text is read
// on a terminal, and a job name carrying ESC or CR can otherwise repaint the line
// so the displayed command no longer matches its bytes.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsFunc(s, unsafeShellChar) {
		return s
	}
	return "'" + strings.ReplaceAll(Escape(s), "'", `'\''`) + "'"
}

// Escape renders C0 control characters and DEL as printable Go-style escapes,
// leaving every other byte untouched. Any free text that reaches the report goes
// through this, whether or not it is shell-quoted.
func Escape(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case isControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// unsafeShellChar reports whether r must be quoted to survive a shell verbatim.
func unsafeShellChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("_@%+=:,./-", r)
}

// RedactAssignment renders a `KEY=VALUE` token with the value masked, for the
// `-v` pairs a submission carries. The key is what a user needs to verify -- that
// the variable is forwarded at all -- and printing the value puts user secrets
// (HF_TOKEN, WANDB_API_KEY) into terminal scrollback and retained CI logs. A token
// with no `=` is returned quoted and unchanged.
func RedactAssignment(kv string) string {
	key, _, ok := strings.Cut(kv, "=")
	if !ok {
		return Quote(kv)
	}
	return Quote(key + "=<value>")
}
