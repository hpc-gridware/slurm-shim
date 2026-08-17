package fabricator

import "strings"

// shellQuote wraps a value in single quotes, replacing each embedded quote with
// the POSIX-safe sequence quote-backslash-quote-quote so a value containing a
// quote, space, or metacharacter cannot break out of `eval` (REQ-ENV-002, SI-03).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sanitizeFreeText makes a cosmetic value (job name, submit dir) safe as a
// SLURM_* value without failing the job (SI-03): any character outside a small
// readable whitelist becomes '_'. It never turns a real name into Lightning's
// interactive sentinels ("bash"/"interactive").
func sanitizeFreeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '+' || r == '-' || r == '/':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// machineValueOK reports whether a machine-consumed value (nodelist, counts,
// ids, ports) contains only characters the strict whitelist permits: alnum plus
// the encoder-output set. It is a defense-in-depth self-test; the shim's own
// encoders always produce conforming output (REQ-FAB-006).
func machineValueOK(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune("[](),.:=_-", r):
		default:
			return false
		}
	}
	return true
}
