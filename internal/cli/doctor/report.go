package doctor

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// SchemaVersion is the version of the `doctor --json` document; it is bumped
// only when a field changes meaning or goes away, never for an added field.
const SchemaVersion = 1

// Finding levels, as the text output prints them.
const (
	LevelPass = "pass"
	LevelWarn = "warn"
	LevelFail = "fail"
	LevelInfo = "info"
)

// Document is `slurm-shim doctor --json`: the same checks and verdicts as the
// text report, for tools (Qontrol's Analyze view) that must not parse text.
//
// A report that did not run every check never looks like a clean one:
// Complete is false and StoppedIn names the section when a run stopped early,
// and Skipped names every section --offline did not run.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	ShimVersion   string    `json:"shim_version"`
	Offline       bool      `json:"offline"`
	Complete      bool      `json:"complete"`
	StoppedIn     string    `json:"stopped_in"`
	Sections      []Section `json:"sections"`
	Skipped       []Skipped `json:"skipped"`
	Summary       Summary   `json:"summary"`
}

// Section is one `== name` block of the report.
type Section struct {
	Name     string    `json:"name"`
	Findings []Finding `json:"findings"`
}

// Finding is one report line. Messages carry names from the cluster
// configuration (queues, PEs, hosts, paths): a consumer escapes them before
// rendering them as markup.
type Finding struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Skipped is a section that did not run, and why.
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Summary counts the FAIL and WARN findings.
type Summary struct {
	Fail int `json:"fail"`
	Warn int `json:"warn"`
}

// report collects the findings of one run. The text report is printed as it
// is found, so a slow or interrupted run still shows everything found so far
// -- what support asks to have pasted; the JSON document can only be written
// once, at the end, so in that mode findings are only collected.
type report struct {
	w         io.Writer
	asJSON    bool
	sections  []Section
	skipped   []Skipped
	stoppedIn string
	fails     int
	warns     int
	offline   bool
}

func newReport(w io.Writer, asJSON, offline bool) *report {
	r := &report{w: w, asJSON: asJSON, offline: offline}
	if !asJSON {
		fmt.Fprintf(w, "slurm-shim doctor  (paste this whole output into a support ticket)\n")
	}
	return r
}

func (r *report) pass(f string, a ...interface{}) { r.add(LevelPass, f, a...) }
func (r *report) warn(f string, a ...interface{}) { r.warns++; r.add(LevelWarn, f, a...) }
func (r *report) fail(f string, a ...interface{}) { r.fails++; r.add(LevelFail, f, a...) }
func (r *report) info(f string, a ...interface{}) { r.add(LevelInfo, f, a...) }

func (r *report) section(name string) {
	r.sections = append(r.sections, Section{Name: name, Findings: []Finding{}})
	if !r.asJSON {
		fmt.Fprintf(r.w, "\n== %s\n", name)
	}
}

func (r *report) add(level, f string, a ...interface{}) {
	if len(r.sections) == 0 {
		r.sections = append(r.sections, Section{Findings: []Finding{}})
	}
	msg := fmt.Sprintf(f, a...)
	last := &r.sections[len(r.sections)-1]
	last.Findings = append(last.Findings, Finding{Level: level, Message: msg})
	if r.asJSON {
		return
	}
	prefix := map[string]string{LevelPass: "PASS  ", LevelWarn: "WARN  ", LevelFail: "FAIL  "}[level]
	if prefix == "" {
		prefix = "      "
	}
	fmt.Fprintf(r.w, "%s%s\n", prefix, msg)
}

// skip records sections that did not run on purpose (--offline): the run is
// still complete, it just covered less. note is the text report's one line.
func (r *report) skip(note, reason string, names ...string) {
	for _, n := range names {
		r.skipped = append(r.skipped, Skipped{Name: n, Reason: reason})
	}
	if !r.asJSON {
		fmt.Fprintf(r.w, "\n== skipped (--offline)\n      %s\n", note)
	}
}

// stop records that the run ended early in the named section; the report is
// then incomplete and the exit code is 1, whatever was found before.
func (r *report) stop(name string) { r.stoppedIn = name }

// finish ends the report -- the summary line, or the whole JSON document -- and
// returns the exit code: 0 without a FAIL, 1 with one or when the run stopped
// early.
func (r *report) finish() int {
	if r.asJSON {
		r.renderJSON(r.w)
	} else {
		fmt.Fprintf(r.w, "\n%d FAIL, %d WARN\n", r.fails, r.warns)
	}
	if r.fails > 0 || r.stoppedIn != "" {
		return 1
	}
	return 0
}

func (r *report) renderJSON(w io.Writer) {
	doc := Document{
		SchemaVersion: SchemaVersion,
		ShimVersion:   version.Shim,
		Offline:       r.offline,
		Complete:      r.stoppedIn == "",
		StoppedIn:     r.stoppedIn,
		Sections:      r.sections,
		Skipped:       r.skipped,
		Summary:       Summary{Fail: r.fails, Warn: r.warns},
	}
	if doc.Sections == nil {
		doc.Sections = []Section{}
	}
	if doc.Skipped == nil {
		doc.Skipped = []Skipped{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc) // a write error to stdout has nowhere better to go
}
