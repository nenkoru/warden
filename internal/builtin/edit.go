package builtin

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Exact-text edits the way oh-my-pi's replace mode does them for Qwen 3.8:
// the exact text first, then the same text with whitespace and typography
// normalized, then a window of lines at least 95% similar and the only one
// that close. A miss names the closest lines (with their line numbers and
// exact text), so the next call can copy them instead of reading the file
// again.

const fuzzyThreshold = 0.95

// edit: content with old replaced by new (every occurrence with all).
// how says which pass matched ("exact", "normalized", "fuzzy NN%").
func edit(content, old, new string, all bool) (out, how string, err error) {
	if old == "" {
		return "", "", fmt.Errorf("old_string is empty: copy the lines to change from the file (read it first)")
	}
	if !strings.Contains(content, old) {
		old = stripLineNumbers(old)
	}
	if n := strings.Count(content, old); n == 1 || (n > 1 && all) {
		return strings.ReplaceAll(content, old, new), "exact", nil
	} else if n > 1 {
		return "", "", fmt.Errorf("old_string occurs %d times (%s). Add a few surrounding lines to make it unique, or set replace_all", n, where(content, old))
	}
	lines := strings.SplitAfter(content, "\n")
	want := strings.Split(strings.TrimRight(old, "\n"), "\n")
	// normalized: per-line trailing space, typography, then leading-indent kind
	if at := matchLines(lines, want, func(a, b string) bool { return norm(a) == norm(b) }); len(at) == 1 {
		return splice(lines, at[0], len(want), reindent(lines[at[0]:], want, new)), "normalized", nil
	} else if len(at) > 1 {
		return "", "", fmt.Errorf("old_string matches %d places once whitespace is ignored (lines %s). Add surrounding lines to make it unique", len(at), lineList(at))
	}
	if len(lines)*len(want) > 300000 {
		return "", "", fmt.Errorf("old_string not found, and the file is too large to look for near matches: grep for a line of it, read that range, and copy the text exactly")
	}
	best, score, second := -1, 0.0, 0.0
	for i := 0; i+len(want) <= len(lines); i++ {
		s := 0.0
		for j := range want {
			s += similarity(norm(lines[i+j]), norm(want[j]))
		}
		s /= float64(len(want))
		if s > score {
			best, second, score = i, score, s
		} else if s > second {
			second = s
		}
	}
	if best >= 0 && score >= fuzzyThreshold && second < fuzzyThreshold {
		return splice(lines, best, len(want), reindent(lines[best:], want, new)), fmt.Sprintf("fuzzy %d%%", int(score*100)), nil
	}
	if best < 0 || score < 0.5 {
		return "", "", fmt.Errorf("old_string not found, nor anything close to it: grep for a line of it, read that range, and copy the text exactly")
	}
	return "", "", fmt.Errorf("old_string not found. The closest text (%d%% similar) is lines %d-%d, exactly:\n%s\nCopy the text to change from there (without the line numbers) and call edit again",
		int(score*100), best+1, best+len(want), numbered(lines[best:best+len(want)], best+1))
}

// matchLines: every start index where want matches lines line by line.
func matchLines(lines, want []string, eq func(a, b string) bool) []int {
	var at []int
	for i := 0; i+len(want) <= len(lines); i++ {
		ok := true
		for j := range want {
			if !eq(lines[i+j], want[j]) {
				ok = false
				break
			}
		}
		if ok {
			at = append(at, i)
		}
	}
	return at
}

// splice: lines with n of them from i replaced by text (ending in a newline
// when the replaced lines did).
func splice(lines []string, i, n int, text string) string {
	if strings.HasSuffix(lines[i+n-1], "\n") && !strings.HasSuffix(text, "\n") && text != "" {
		text += "\n"
	}
	return strings.Join(lines[:i], "") + text + strings.Join(lines[i+n:], "")
}

// reindent: new with the indentation the model wrote (on the old lines)
// replaced by what the file has on the lines they matched (tabs for
// spaces, say), the longest written indent first.
func reindent(file, want []string, new string) string {
	swap := map[string]string{}
	for j, w := range want {
		if strings.TrimSpace(w) != "" {
			swap[indent(w)] = indent(file[j])
		}
	}
	var b strings.Builder
	for _, l := range strings.SplitAfter(new, "\n") {
		best := ""
		for wrote := range swap {
			if len(wrote) > len(best) && strings.HasPrefix(l, wrote) {
				best = wrote
			}
		}
		if have, ok := swap[best]; ok && have != best {
			l = have + strings.TrimPrefix(l, best)
		}
		b.WriteString(l)
	}
	return b.String()
}

func indent(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

var typography = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`, "–", "-", "—", "-", " ", " ", "\r", "")

// norm: a line compared without its indentation, trailing space or
// typographic quotes and dashes, and with runs of blanks as one.
func norm(s string) string {
	return strings.Join(strings.Fields(typography.Replace(s)), " ")
}

// similarity: 1 - the Levenshtein distance over the longer length, in runes.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	prev, cur := make([]int, len(rb)+1), make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			c := 1
			if ra[i-1] == rb[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(max(len(ra), len(rb)))
}

// stripLineNumbers: old text copied with the "N:" prefixes of grep's
// output, which the file does not have.
func stripLineNumbers(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		if l != "" && lineNo(l) < 0 {
			return s
		}
	}
	for i, l := range lines {
		if n := lineNo(l); n >= 0 {
			lines[i] = l[n:]
		}
	}
	return strings.Join(lines, "\n")
}

// lineNo: the length of a leading "  12:" or "12\t", or -1.
func lineNo(l string) int {
	i := len(l) - len(strings.TrimLeft(l, " "))
	j := i
	for j < len(l) && l[j] >= '0' && l[j] <= '9' {
		j++
	}
	if j == i || j >= len(l) || (l[j] != ':' && l[j] != '\t') {
		return -1
	}
	return j + 1
}

// where: the line numbers of each occurrence of s in content (up to 5).
func where(content, s string) string {
	var at []int
	for off := 0; len(at) < 5; {
		k := strings.Index(content[off:], s)
		if k < 0 {
			break
		}
		at = append(at, strings.Count(content[:off+k], "\n"))
		off += k + max(1, len(s))
	}
	return "lines " + lineList(at)
}

func lineList(at []int) string {
	s := make([]string, len(at))
	for i, a := range at {
		s[i] = fmt.Sprint(a + 1)
	}
	return strings.Join(s, ", ")
}

// numbered: lines prefixed "N:" from first, for an error message.
func numbered(lines []string, first int) string {
	var b strings.Builder
	for i, l := range lines {
		if !utf8.ValidString(l) {
			l = strings.ToValidUTF8(l, "?")
		}
		fmt.Fprintf(&b, "%d:%s", first+i, strings.TrimRight(l, "\n")+"\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
