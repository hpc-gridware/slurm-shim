// Package sbatch implements the sbatch shim (spec sec. 7.6): it parses #SBATCH
// directives from the submitted script, translates a partition to a GE queue +
// parallel environment + slot count, submits with qsub -terse, and prints
// "Submitted batch job <id>". clearml's SLURM glue renders a site template of
// #SBATCH directives rather than passing fixed CLI flags (REQ-SBT-001).
package sbatch

import "strings"

// ParseDirectives extracts the option tokens from a script's #SBATCH (and
// #SHIM) directives (REQ-SBT-001). Directives are read from the top of the script: an optional
// shebang, then lines that are blank or comments, up to the first executable
// line, which stops directive scanning (matching SLURM). Each `#SBATCH <args>`
// line contributes its whitespace-split tokens in order.
func ParseDirectives(script []byte) []string {
	var tokens []string
	lines := strings.Split(string(script), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if i == 0 && strings.HasPrefix(line, "#!") {
			continue // shebang
		}
		if rest, ok := directiveArgs(line); ok {
			tokens = append(tokens, tokenizeDirective(rest)...)
			continue
		}
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			continue // ordinary comment between directives
		default:
			return tokens // first executable line ends the directive block
		}
	}
	return tokens
}

// directiveArgs returns the argument text of a #SBATCH or #SHIM line. #SHIM carries
// shim-only options (--x-spares, --x-elastic) for scripts that must also run on
// real SLURM, which rejects an unknown #SBATCH option but ignores this line as a
// comment; it is read exactly like #SBATCH. It must be followed by whitespace or
// end the line, so a comment such as '#SHIMX foo' stays a comment.
func directiveArgs(line string) (string, bool) {
	if rest, ok := strings.CutPrefix(line, "#SBATCH"); ok {
		return rest, true
	}
	if rest, ok := strings.CutPrefix(line, "#SHIM"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
		return rest, true
	}
	return "", false
}

// tokenizeDirective splits a directive's argument text into tokens, honoring
// single and double quotes so a value with spaces (e.g. --job-name="my job")
// stays one token. Quotes are removed from the emitted token. Backslash escapes
// the next character outside single quotes.
//
// An unquoted, unescaped '#' starts a comment that runs to the end of the line,
// as in SLURM's own directive parser: `#SBATCH -N 4   # node count` is common in
// site templates. Emitting the comment's words as tokens was worse than noise --
// the first of them is a positional argument, which ends flag parsing, so every
// later directive and every command-line flag was silently dropped.
func tokenizeDirective(s string) []string {
	var tokens []string
	var cur strings.Builder
	inWord := false
	var quote byte // 0, '\'' or '"'
scan:
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
			inWord = true
		case c == '#':
			break scan
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
