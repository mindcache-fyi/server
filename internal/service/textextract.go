package service

import (
	"strings"
	"unicode"
)

// ExtractPlainText reduces markdown to the text a human reads so the search
// index matches prose rather than syntax. Code blocks keep their inner text
// (searching code is useful) but lose fence markers; links keep anchor text
// and drop URLs; emphasis/list/heading markers are removed.
func ExtractPlainText(markdown string) string {
	lines := strings.Split(markdown, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		out = append(out, stripInlineMarkdown(strings.TrimLeft(line, " \t")))
	}
	return normalizeSpaces(strings.Join(out, "\n"))
}

// stripInlineMarkdown removes common inline markdown syntax from one line.
func stripInlineMarkdown(line string) string {
	if isHeadingLine(line) {
		line = strings.TrimLeft(line, "#")
		line = strings.TrimLeft(line, " ")
	}
	if marker, ok := listItemMarker(line); ok {
		line = line[len(marker):]
	}

	var b strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '\\':
			// Escaped character: keep the escaped rune itself.
			if i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
			}
		case '`':
			// Inline code: drop backticks, keep contents.
		case '*', '_':
			// Emphasis markers: drop runs of two or more (bold), and single
			// markers that act as delimiters; keep literal ones inside words
			// such as snake_case names.
			j := i
			for j < len(runes) && runes[j] == r {
				j++
			}
			if j-i >= 2 || isEmphasisDelimiter(runes, i) {
				i = j - 1
				continue
			}
			b.WriteRune(r)
		case '!':
			// Image: ![alt](url) → alt. Drop the bang and let the link
			// branch below keep the anchor text.
			if i+1 < len(runes) && runes[i+1] == '[' {
				continue
			}
			b.WriteRune(r)
		case '[':
			anchor, rest, ok := cutLink(runes[i:])
			if ok {
				b.WriteString(anchor)
				i += len(runes[i:]) - len(rest) - 1
			} else {
				b.WriteRune(r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isHeadingLine reports whether the line starts an ATX heading (#, ##, …).
func isHeadingLine(line string) bool {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	return n > 0 && n <= 6 && (n == len(line) || line[n] == ' ')
}

// listItemMarker returns the bullet or ordered-list prefix of line.
func listItemMarker(line string) (string, bool) {
	for _, m := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, m) {
			return m[:1], true
		}
	}
	// Ordered list: digits followed by '.' or ')'.
	n := 0
	for n < len(line) && line[n] >= '0' && line[n] <= '9' {
		n++
	}
	if n > 0 && n < len(line) && (line[n] == '.' || line[n] == ')') &&
		(n+1 == len(line) || line[n+1] == ' ') {
		return line[:n+1], true
	}
	return "", false
}

// isEmphasisDelimiter reports whether runes[i] opens or closes emphasis
// rather than being a literal asterisk/underscore (e.g. snake_case names).
func isEmphasisDelimiter(runes []rune, i int) bool {
	prevSpace := i == 0 || unicode.IsSpace(runes[i-1]) || isPunct(runes[i-1])
	nextSpace := i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) || isPunct(runes[i+1])
	// Opening delimiter: preceded by space/punctuation, followed by content.
	if prevSpace && !nextSpace {
		return true
	}
	// Closing delimiter: preceded by content, followed by space/punctuation.
	if !prevSpace && nextSpace {
		return true
	}
	return prevSpace && nextSpace // lone marker between whitespace
}

func isPunct(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// cutLink splits "[anchor](target)" at the top of src. It returns the anchor
// text and whether src started with a well-formed link.
func cutLink(src []rune) (anchor string, rest []rune, ok bool) {
	depth := 0
	for i, r := range src {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 && i+1 < len(src) && src[i+1] == '(' {
				close := indexRune(src[i+2:], ')')
				if close < 0 {
					return "", nil, false
				}
				end := i + 2 + close + 1
				return string(src[1:i]), src[end:], true
			}
			return "", nil, false
		}
	}
	return "", nil, false
}

func indexRune(runes []rune, target rune) int {
	for i, r := range runes {
		if r == target {
			return i
		}
	}
	return -1
}

// normalizeSpaces collapses runs of spaces/tabs inside lines and trims blank
// line runs so repeated words are not inflated by formatting.
func normalizeSpaces(s string) string {
	var b strings.Builder
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			b.WriteString(strings.Join(fields, " "))
		}
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
