package yaml

import "fmt"

// scalarSpan describes the exact bytes replaced for one scalar.
type scalarSpan struct {
	// start/end delimit the replaced byte range of source.
	start, end int
	// targetStart is the start of the scalar itself (equal to start except
	// for block scalars whose header carries a trailing comment).
	targetStart int

	// block metadata (zero for non-block styles).
	blockKind     byte   // '|' or '>'
	parentIndent  int    // indentation of the parent node (0-based columns)
	contentIndent int    // measured indentation of block content (0 when empty)
	explicitDigit bool   // header carried an explicit indentation digit
	chomp         byte   // 0, '-' or '+'
	nl            string // physical line ending of the header line ("\n" or "\r\n")
	headerComment string // comment text including the leading '#', if any
	empty         bool

	// implicitNull marks a value-less scalar ("key:"); the replaced bytes are
	// the whitespace between ':'/'-' and the comment or line end.
	implicit         bool
	needsTrailingGap bool
}

func (s scalarSpan) len() int { return s.end - s.start }

// endsWithLineBreak reports whether the last byte of the replaced span is a
// physical line ending.
func endsWithLineBreak(src []byte, s scalarSpan) bool {
	if s.end <= s.start || s.end > len(src) {
		return false
	}
	return src[s.end-1] == '\n' || src[s.end-1] == '\r'
}

func scanScalarSpan(source []byte, style scalarStyle, start int) (scalarSpan, error) {
	switch style {
	case stylePlain:
		return scanPlainSpan(source, start)
	case styleSingleQuote:
		return scanQuotedSpan(source, start, '\'')
	case styleDoubleQuote:
		return scanQuotedSpan(source, start, '"')
	case styleLiteral, styleFolded:
		return scanBlockSpan(source, start)
	}
	return scalarSpan{}, ErrScalarNodeRequired
}

// scanImplicitNullSpan covers a value-less mapping entry ("a:") or sequence
// entry ("-"). start points at the ':' or '-' indicator.
func scanImplicitNullSpan(src []byte, start int) (scalarSpan, error) {
	insert := start + 1
	for insert < len(src) && (src[insert] == ' ' || src[insert] == '\t') {
		insert++
	}
	trailing := insert < len(src) && src[insert] == '#'
	return scalarSpan{
		start:            start + 1,
		end:              insert,
		targetStart:      start,
		implicit:         true,
		needsTrailingGap: trailing,
		nl:               "\n",
	}, nil
}

// scanPlainSpan scans a plain scalar starting at start.
func scanPlainSpan(src []byte, start int) (scalarSpan, error) {
	flowDepth := flowDepthAt(src, start)
	i := start
	for i < len(src) {
		b := src[i]
		switch b {
		case '\n', '\r':
			// A plain scalar cannot span lines in block context. In flow
			// context a folded line continuation would be required, which
			// goccy/go-yaml emits as a single-line token; treat the break as
			// the end and let re-parse verification decide.
			goto done
		case '#':
			// '#' only starts a comment after whitespace.
			if i > start && (src[i-1] == ' ' || src[i-1] == '\t') {
				goto done
			}
		case ':', '-':
			if isPlainIndicatorAt(src, i, b, flowDepth) {
				goto done
			}
		case ',', '[', ']', '{', '}':
			if flowDepth > 0 {
				goto done
			}
		}
		i++
	}
done:
	end := i
	// Trim trailing spaces/tabs: the scalar never owns them.
	for end > start && (src[end-1] == ' ' || src[end-1] == '\t') {
		end--
	}
	if end < start {
		end = start
	}
	return scalarSpan{start: start, end: end, targetStart: start}, nil
}

// isPlainIndicatorAt reports whether the ':' or '-' at index i terminates a
// plain scalar.
func isPlainIndicatorAt(src []byte, i int, b byte, flowDepth int) bool {
	if b == ':' {
		next := byteAt(src, i+1)
		if next == ' ' || next == '\t' || next == '\n' || next == '\r' || i+1 == len(src) {
			return true
		}
		if flowDepth > 0 && (next == ',' || next == ']' || next == '}') {
			return true
		}
		return false
	}
	// '-' only counts as a sequence entry at the start of an entry.
	prev := byteAt(src, i-1)
	if (prev == ' ' || prev == '\t' || i == 0 || prev == '\n' || prev == '\r') &&
		(byteAt(src, i+1) == ' ' || byteAt(src, i+1) == '\n' || i+1 == len(src)) {
		return true
	}
	return false
}

func byteAt(src []byte, i int) byte {
	if i < 0 || i >= len(src) {
		return 0
	}
	return src[i]
}

// flowDepthAt counts unclosed flow indicators ([, {) from the document start
// to start, ignoring quoted text and comments.
func flowDepthAt(src []byte, start int) int {
	depth := 0
	for i := 0; i < start; i++ {
		switch src[i] {
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '\'', '"':
			q := src[i]
			i++
			for i < start && src[i] != q {
				if src[i] == '\\' && q == '"' {
					i++
				}
				i++
			}
		case '#':
			for i < start && src[i] != '\n' && src[i] != '\r' {
				i++
			}
		}
	}
	return depth
}

// scanQuotedSpan scans a single- or double-quoted scalar from the opening
// quote through the closing quote.
func scanQuotedSpan(src []byte, start int, quote byte) (scalarSpan, error) {
	if start >= len(src) || src[start] != quote {
		return scalarSpan{}, fmt.Errorf("%w: expected %q at offset %d", ErrScalarPositionMismatch, quote, start)
	}
	i := start + 1
	for i < len(src) {
		switch src[i] {
		case '\\':
			if quote == '"' && i+1 < len(src) {
				i += 2
				continue
			}
		case quote:
			if quote == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					i += 2
					continue
				}
			}
			return scalarSpan{start: start, end: i + 1, targetStart: start}, nil
		case '\n', '\r':
			break
		}
		i++
	}
	return scalarSpan{}, fmt.Errorf("%w: unterminated quoted scalar at offset %d", ErrScalarPositionMismatch, start)
}

type physicalLine struct {
	start, end int // [start, end) without the line ending
	ending     string
}

func lineAt(src []byte, idx int) physicalLine {
	s := idx
	for s > 0 && src[s-1] != '\n' && src[s-1] != '\r' {
		s--
	}
	e := idx
	for e < len(src) && src[e] != '\n' && src[e] != '\r' {
		e++
	}
	ending := ""
	if e < len(src) {
		if src[e] == '\r' && e+1 < len(src) && src[e+1] == '\n' {
			ending = "\r\n"
		} else {
			ending = string(src[e])
		}
	}
	return physicalLine{start: s, end: e, ending: ending}
}

// scanBlockSpan scans a literal (|) or folded (>) scalar starting at the
// indicator character.
func scanBlockSpan(src []byte, start int) (scalarSpan, error) {
	if start >= len(src) || (src[start] != '|' && src[start] != '>') {
		return scalarSpan{}, fmt.Errorf("%w: expected block indicator at offset %d", ErrScalarPositionMismatch, start)
	}
	header := lineAt(src, start)

	span := scalarSpan{
		start:       start,
		targetStart: start,
		blockKind:   src[start],
		nl:          header.ending,
	}
	if span.nl == "" {
		span.nl = "\n"
	}

	// Parse modifiers: an optional indentation digit (1-9) and one chomping
	// indicator (- or +), in either order.
	i := start + 1
	modifiers := true
	for i < header.end {
		c := src[i]
		switch {
		case c == ' ' || c == '\t':
			modifiers = false
		case c >= '1' && c <= '9':
			if !modifiers || span.explicitDigit {
				break
			}
			span.contentIndent = int(c - '0')
			span.explicitDigit = true
		case c == '-' || c == '+':
			if !modifiers || span.chomp != 0 {
				break
			}
			span.chomp = c
		case c == '#':
			modifiers = false
			span.headerComment = string(src[i:header.end])
			i = header.end
			continue
		default:
			modifiers = false
		}
		i++
	}
	if !span.explicitDigit {
		span.contentIndent = 0
	}

	// Parent indentation comes from the column of the indicator.
	span.parentIndent = start - header.start

	headerEnd := header.end + len(header.ending)

	// Walk physical lines after the header.
	type ln struct {
		indent int
		blank  bool
		line   physicalLine
	}
	var lines []ln
	pos := headerEnd
	for pos < len(src) {
		l := lineAt(src, pos)
		ind := 0
		for ind < l.end-l.start && src[l.start+ind] == ' ' {
			ind++
		}
		// A line containing only spaces/tabs is blank and remains part of
		// the block regardless of its (space-only) indentation.
		blank := true
		for j := l.start; j < l.end; j++ {
			if src[j] != ' ' && src[j] != '\t' {
				blank = false
				break
			}
		}
		lines = append(lines, ln{indent: ind, blank: blank, line: l})
		if l.ending == "" {
			break
		}
		pos = l.end + len(l.ending)
	}

	if !span.explicitDigit {
		for _, x := range lines {
			if !x.blank {
				span.contentIndent = x.indent
				break
			}
		}
		if span.contentIndent == 0 {
			span.contentIndent = span.parentIndent + 1
			span.empty = true
		}
	} else {
		// Explicit digit is relative to parent indentation.
		span.contentIndent = span.parentIndent + span.contentIndent
		anyContent := false
		for _, x := range lines {
			if !x.blank {
				anyContent = true
			}
		}
		span.empty = !anyContent
	}

	// Content lines are blank lines plus non-blank lines indented at least
	// contentIndent. The first line with content at lower indentation ends
	// the scalar.
	contentCount := 0
	for _, x := range lines {
		if x.blank || x.indent >= span.contentIndent {
			contentCount++
			continue
		}
		break
	}

	if contentCount == 0 {
		// Empty block: span ends right after the header line ending.
		span.end = headerEnd
		return span, nil
	}

	content := lines[:contentCount]

	// Trim trailing blank lines for clip ('') and strip ('-'); keep ('+')
	// retains them as part of the scalar.
	trailing := 0
	if span.chomp != '+' {
		for trailing < len(content) && content[len(content)-1-trailing].blank {
			trailing++
		}
	}
	kept := content[:len(content)-trailing]
	last := kept[len(kept)-1].line
	// The span runs through the line break of the last physical content
	// line. That break is structural, but belongs to the scalar: replacing
	// it away would splice following tokens directly onto the content.
	if last.ending != "" {
		span.end = last.end + len(last.ending)
	} else {
		span.end = last.end
	}
	return span, nil
}
