package gedata

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// OCSBuild is the scheduler's version as its clients report it: the release
// (e.g. "9.1.5") and the build stamp in parentheses (e.g. "250826-0734"). The
// stamp matters because fixes land in builds: the qacct exit_status fix the shim
// depends on for sacct is in 9.1.5 (250826-0734) specifically.
type OCSBuild struct {
	Release string // "9.1.5"
	Stamp   string // "250826-0734", "" when the client prints none
	Raw     string // the line as printed
}

// versionLine matches the first line every OCS/GE client prints for -help:
// "OCS 9.1.5 (250826-0734)", "GCS 9.1.5 (...)", or classic "SGE 8.1.9".
var versionLine = regexp.MustCompile(`^\s*(?:OCS|GCS|SGE|GE|UGE)\s+(\d+(?:\.\d+)*)(?:\s+\(([^)]*)\))?`)

// OCSVersion reports the scheduler build by asking qconf, which every install
// has and which needs no qmaster round trip for -help.
func OCSVersion(ctx context.Context, r Runner) (OCSBuild, error) {
	out, errOut, _, err := r.Run(ctx, "qconf", "-help")
	if err != nil {
		return OCSBuild{}, fmt.Errorf("qconf -help: %w", err)
	}
	// Some clients print -help to stderr; take whichever has the banner.
	for _, text := range []string{string(out), string(errOut)} {
		for _, line := range strings.Split(text, "\n") {
			if m := versionLine.FindStringSubmatch(line); m != nil {
				return OCSBuild{Release: m[1], Stamp: m[2], Raw: strings.TrimSpace(line)}, nil
			}
		}
	}
	return OCSBuild{}, fmt.Errorf("qconf -help printed no recognisable version line")
}

// AtLeast reports whether the build is at or after the given release and,
// when both carry one, build stamp. Stamps are YYMMDD-HHMM, so string order is
// chronological. A build with no stamp compares on release alone.
func (b OCSBuild) AtLeast(release, stamp string) bool {
	c := compareRelease(b.Release, release)
	if c != 0 {
		return c > 0
	}
	if stamp == "" || b.Stamp == "" {
		return true
	}
	return b.Stamp >= stamp
}

// compareRelease compares dotted release strings numerically per component.
func compareRelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// String renders the build the way the clients print it.
func (b OCSBuild) String() string {
	if b.Stamp == "" {
		return b.Release
	}
	return b.Release + " (" + b.Stamp + ")"
}
