package submit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BeginStamp formats a start time the way `qsub -a` takes it:
// [[CC]YY]MMDDhhmm[.SS] (verified on OCS 9.1.6).
func BeginStamp(t time.Time) string { return t.Format("200601021504.05") }

var (
	relativeBegin = regexp.MustCompile(`^now(?:\+(\d+)([a-z]*))?$`)
	clockBegin    = regexp.MustCompile(`^(\d{1,2}):(\d{2})(?::(\d{2}))?\s*(am|pm)?$`)
	compactDate   = regexp.MustCompile(`^(\d{2})(\d{2})(\d{2})?$`)
	slashDate     = regexp.MustCompile(`^(\d{1,2})[/.](\d{1,2})(?:[/.](\d{2}))?$`)
	isoDate       = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:t(\d{1,2}):(\d{2})(?::(\d{2}))?)?$`)
)

// ParseBegin reads a SLURM --begin value relative to now, in now's location.
// The forms are SLURM 24.05's: now[+count[unit]] (seconds by default),
// HH:MM[:SS] [AM|PM] (today, or tomorrow once past), the named times
// midnight, noon, elevenses, fika and teatime (the next one), today and
// tomorrow, MMDD[YY], MM/DD[/YY] and MM.DD[.YY], a date followed by
// -HH:MM[:SS], and YYYY-MM-DD[THH:MM[:SS]].
func ParseBegin(val string, now time.Time) (time.Time, error) {
	v := strings.ToLower(strings.TrimSpace(val))
	midnight := clockOn(now, 0, 0, 0)
	nextAt := func(h, mi, s int) time.Time {
		t := clockOn(now, h, mi, s)
		if !t.After(now) {
			t = clockOn(midnight.AddDate(0, 0, 1), h, mi, s)
		}
		return t
	}
	switch v {
	case "today":
		return midnight, nil
	case "tomorrow":
		return midnight.AddDate(0, 0, 1), nil
	case "midnight":
		return midnight.AddDate(0, 0, 1), nil
	case "noon":
		return nextAt(12, 0, 0), nil
	case "elevenses":
		return nextAt(11, 0, 0), nil
	case "fika":
		return nextAt(15, 0, 0), nil
	case "teatime":
		return nextAt(16, 0, 0), nil
	}
	if m := relativeBegin.FindStringSubmatch(v); m != nil {
		return relative(now, m[1], m[2], val)
	}
	if m := isoDate.FindStringSubmatch(v); m != nil {
		return isoBegin(m, now.Location(), val)
	}
	if date, clock, ok := strings.Cut(v, "-"); ok {
		day, err := dateBegin(date, now, val)
		if err != nil {
			return time.Time{}, err
		}
		h, mi, s, err := clockParts(clock, val)
		if err != nil {
			return time.Time{}, err
		}
		return clockOn(day, h, mi, s), nil
	}
	if clockBegin.MatchString(v) {
		h, mi, s, err := clockParts(v, val)
		if err != nil {
			return time.Time{}, err
		}
		return nextAt(h, mi, s), nil
	}
	return dateBegin(v, now, val)
}

// clockOn returns the wall-clock time h:mi:s on day's date. It is built with
// time.Date, not added to midnight: on a daylight-saving change day a
// duration from midnight lands an hour off the clock time asked for.
func clockOn(day time.Time, h, mi, s int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), h, mi, s, 0, day.Location())
}

// maxBeginOffsetDays bounds now+N: a larger offset overflows time.Duration
// into the past, or writes a year qsub -a cannot take.
const maxBeginOffsetDays = 100 * 365

func relative(now time.Time, count, unit, val string) (time.Time, error) {
	if count == "" {
		return now, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return time.Time{}, fmt.Errorf("--begin=%s: invalid count", val)
	}
	var perUnit time.Duration
	days := 0
	switch unit {
	case "", "s", "sec", "secs", "second", "seconds":
		perUnit = time.Second
	case "m", "min", "mins", "minute", "minutes":
		perUnit = time.Minute
	case "h", "hour", "hours":
		perUnit = time.Hour
	case "d", "day", "days":
		days = 1
	case "w", "week", "weeks":
		days = 7
	default:
		return time.Time{}, fmt.Errorf("--begin=%s: unknown unit %q", val, unit)
	}
	tooFar := fmt.Errorf("--begin=%s: more than %d years ahead", val, maxBeginOffsetDays/365)
	if days > 0 {
		if n > maxBeginOffsetDays/days {
			return time.Time{}, tooFar
		}
		return now.AddDate(0, 0, days*n), nil
	}
	if int64(n) > int64(maxBeginOffsetDays)*int64(24*time.Hour/perUnit) {
		return time.Time{}, tooFar
	}
	return now.Add(time.Duration(n) * perUnit), nil
}

func isoBegin(m []string, loc *time.Location, val string) (time.Time, error) {
	n := func(s string) int { i, _ := strconv.Atoi(s); return i }
	y, mo, d := n(m[1]), n(m[2]), n(m[3])
	h, mi, s := n(m[4]), n(m[5]), n(m[6])
	day, ok := validDate(y, mo, d, loc)
	if !ok || h > 23 || mi > 59 || s > 59 {
		return time.Time{}, fmt.Errorf("--begin=%s: invalid date or time", val)
	}
	return clockOn(day, h, mi, s), nil
}

// dateBegin reads MMDD[YY], MM/DD[/YY] or MM.DD[.YY] as the start of that day.
func dateBegin(v string, now time.Time, val string) (time.Time, error) {
	m := compactDate.FindStringSubmatch(v)
	if m == nil {
		m = slashDate.FindStringSubmatch(v)
	}
	if m == nil {
		return time.Time{}, fmt.Errorf("--begin=%s: unrecognized time; use e.g. now+1hour, 16:00, "+
			"tomorrow or 2026-10-12T16:00", val)
	}
	mo, _ := strconv.Atoi(m[1])
	d, _ := strconv.Atoi(m[2])
	y := now.Year()
	if m[3] != "" {
		yy, _ := strconv.Atoi(m[3])
		y = 2000 + yy
	}
	t, ok := validDate(y, mo, d, now.Location())
	if !ok {
		return time.Time{}, fmt.Errorf("--begin=%s: invalid date", val)
	}
	// A date without a year that has passed is next year's, as in SLURM:
	// "0105" written in December means the coming January, not a job that
	// starts at once.
	if m[3] == "" && t.Before(clockOn(now, 0, 0, 0)) {
		if t, ok = validDate(y+1, mo, d, now.Location()); !ok {
			return time.Time{}, fmt.Errorf("--begin=%s: invalid date", val)
		}
	}
	return t, nil
}

// validDate builds the start of y-mo-d, ok false for a date that does not
// exist (time.Date would roll 02/30 over into March).
func validDate(y, mo, d int, loc *time.Location) (time.Time, bool) {
	if mo < 1 || mo > 12 || d < 1 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
	return t, t.Day() == d
}

// clockParts reads HH:MM[:SS] [AM|PM].
func clockParts(v, val string) (h, mi, s int, err error) {
	m := clockBegin.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, 0, 0, fmt.Errorf("--begin=%s: invalid time of day", val)
	}
	h, _ = strconv.Atoi(m[1])
	mi, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		s, _ = strconv.Atoi(m[3])
	}
	switch m[4] {
	case "am":
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 12 {
			h += 12
		}
	}
	if h > 23 || mi > 59 || s > 59 {
		return 0, 0, 0, fmt.Errorf("--begin=%s: invalid time of day", val)
	}
	return h, mi, s, nil
}
