// Package sbatch implements the sbatch shim (spec sec. 7.6): it parses #SBATCH
// directives from the submitted script, translates a partition to a GE queue +
// parallel environment + slot count, submits with qsub -terse, and prints
// "Submitted batch job <id>". clearml's SLURM glue renders a site template of
// #SBATCH directives rather than passing fixed CLI flags (REQ-SBT-001).
package sbatch

import "strings"

// ParseDirectives extracts the option tokens from a script's #SBATCH directives
// (REQ-SBT-001). Directives are read from the top of the script: an optional
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
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#SBATCH"):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "#SBATCH"))
			tokens = append(tokens, tokenizeDirective(rest)...)
		case strings.HasPrefix(line, "#"):
			continue // ordinary comment between directives
		default:
			return tokens // first executable line ends the directive block
		}
	}
	return tokens
}

// tokenizeDirective splits a directive's argument text into tokens, honoring
// single and double quotes so a value with spaces (e.g. --job-name="my job")
// stays one token. Quotes are removed from the emitted token. Backslash escapes
// the next character outside single quotes.
func tokenizeDirective(s string) []string {
	var tokens []string
	var cur strings.Builder
	inWord := false
	var quote byte // 0, '\'' or '"'
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
