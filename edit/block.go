package edit

import (
	"strconv"
	"strings"
)

// blockLayout describes a located block scalar region.
type blockLayout struct {
	// indicatorOff is the byte offset of the '|' or '>' character.
	indicatorOff int
	// headerEndOff is the byte offset just after the header options and
	// spaces on the header line (before a comment or line break).
	headerEndOff int
	// lineBreak is the line break sequence used after the header.
	lineBreak string
	// hasBreak reports whether the header line had a terminating break.
	hasBreak bool
	// contentIndent is the 1-based column where content begins.
	contentIndent int
	// endOff is the half-open end of the block lexical range.
	endOff int
	// hasSibling reports whether non blank structure follows the block.
	hasSibling bool
}

// replaceBlock computes the range and replacement for a literal or
// folded block scalar.
func replaceBlock(src string, lines []lineInfo, tgt *target, value string) (ScalarRange, []byte, error) {
	layout, ok := locateBlock(src, lines, tgt)
	if !ok {
		return ScalarRange{}, nil, ErrPositionMismatch
	}
	rng := rangeOf(lines, layout.indicatorOff, layout.endOff)
	replacement, ok := renderBlock(value, tgt.style, src, lines, layout)
	if !ok {
		return ScalarRange{}, nil, ErrUnsupportedNode
	}
	return rng, []byte(replacement), nil
}

// locateBlock finds the header indicator and the extent of block content.
func locateBlock(src string, lines []lineInfo, tgt *target) (blockLayout, bool) {
	tk := tgt.header
	start := colOffset(lines, tk.Position.Line, tk.Position.Column)
	if start < 0 || start >= len(src) {
		return blockLayout{}, false
	}
	var indicator byte
	switch tgt.style {
	case styleLiteral:
		indicator = '|'
	case styleFolded:
		indicator = '>'
	default:
		return blockLayout{}, false
	}
	if src[start] != indicator {
		return blockLayout{}, false
	}

	headerLineIdx := tk.Position.Line - 1
	headerLine := lines[headerLineIdx]

	l := blockLayout{
		indicatorOff: start,
		lineBreak:    "\n",
	}

	// Scan header options; stop before a same line comment.
	i := start + 1
	lineEnd := headerLine.start + len(headerLine.text)
	for i < lineEnd && src[i] != '#' {
		i++
	}
	l.headerEndOff = i

	if headerLine.brk == "" {
		l.hasBreak = false
		l.contentIndent = defaultBlockIndent(headerLine.text)
		l.endOff = lineEnd
		return l, true
	}
	l.hasBreak = true
	l.lineBreak = headerLine.brk

	indent := blockExplicitIndent(tk.Value)
	firstContent := -1
	siblingLine := -1
	for idx := headerLineIdx + 1; idx < len(lines); idx++ {
		cur := lines[idx]
		sp := countLeadingSpaces(cur.text)
		if sp == len(cur.text) {
			continue
		}
		// A following line that is a block structure indicator ("- ",
		// "? ") or sits at column 1 cannot be block content; the block is
		// empty.
		if (indent == 0 && sp == 0) || isBlockIndicatorAt(cur.text, sp) {
			siblingLine = idx
			break
		}
		if indent == 0 {
			indent = sp // auto detection: indicator reports leading spaces
		}
		firstContent = idx
		break
	}
	if firstContent < 0 {
		l.contentIndent = nonZero(indent, defaultBlockIndent(headerLine.text))
		if siblingLine >= 0 {
			// First following structure line is a sibling; the block is
			// empty and its range ends right where that sibling starts.
			l.endOff = lines[siblingLine].start
			l.hasSibling = true
			return l, true
		}
		l.endOff = len(src)
		return l, true
	}
	l.contentIndent = indent

	endLine := -1
	for idx := firstContent + 1; idx < len(lines); idx++ {
		cur := lines[idx]
		sp := countLeadingSpaces(cur.text)
		if sp == len(cur.text) {
			continue
		}
		if sp < indent || (sp == 0 && isDocumentMarker(cur.text)) {
			endLine = idx
			break
		}
	}
	if endLine < 0 {
		l.endOff = len(src)
	} else {
		l.endOff = lines[endLine].start
		l.hasSibling = true
	}
	return l, true
}

// blockExplicitIndent extracts an explicit indentation digit from a
// header value such as "|2" or ">+2".
func blockExplicitIndent(header string) int {
	for i := 1; i < len(header); i++ {
		if c := header[i]; c >= '1' && c <= '9' {
			return int(c - '0')
		}
	}
	return 0
}

// defaultBlockIndent predicts content indentation for an empty block from
// the header line's own indentation.
func defaultBlockIndent(headerText string) int {
	// Header indicator sits after headerSpaces spaces. New mapping content
	// normally begins two spaces deeper than the key. Sequence entries
	// ("  - |") need content aligned with the indicator (2 spaces before
	// it plus two more), so two spaces beyond the indicator works in both.
	return countLeadingSpaces(headerText) + 2
}

func nonZero(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// renderBlock renders value as a block scalar over the located region.
//
// value == "" is represented by a quoted scalar because block style
// cannot express an empty value on the same header line losslessly.
func renderBlock(value string, style scalarStyle, src string, lines []lineInfo, l blockLayout) (string, bool) {
	trailingComment := headerTrailingComment(src, lines, l)

	if value == "" {
		return renderEmptyBlock(trailingComment, l), true
	}

	// Folded style must never fold new value line breaks into spaces.
	effStyle := style
	if style == styleFolded && strings.Contains(value, "\n") {
		effStyle = styleLiteral
	}
	indicator := '|'
	if effStyle == styleFolded {
		indicator = '>'
	}

	contentIndent := l.contentIndent
	if contentIndent <= 0 {
		contentIndent = 2
	}
	bodyLines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	// contentIndent is the number of leading spaces before content. When
	// the first line itself begins with spaces, auto detection cannot tell
	// indentation from value spaces, so an explicit indicator is needed.
	leading := countLeadingSpaces(bodyLines[0])
	needExplicit := leading > 0
	if needExplicit && contentIndent+leading > 9 {
		return blockInlineFallback(value, l), true
	}

	var sb strings.Builder
	sb.WriteRune(indicator)
	if needExplicit {
		sb.WriteString(strconv.Itoa(contentIndent))
	}
	switch trailingNewlineCount(value) {
	case 0:
		sb.WriteByte('-')
	case 1:
		// clip: no chomping indicator
	default:
		sb.WriteByte('+')
	}
	if trailingComment != "" {
		sb.WriteByte(' ')
		sb.WriteString(trailingComment)
	}
	if !l.hasBreak {
		// Header was the final line with no break; nothing more to add.
		return sb.String(), true
	}
	sb.WriteString(l.lineBreak)

	prefix := strings.Repeat(" ", contentIndent)
	for i, ln := range bodyLines {
		if i > 0 {
			sb.WriteString(l.lineBreak)
		}
		if ln != "" {
			sb.WriteString(prefix)
			sb.WriteString(ln)
		}
	}

	for i := 0; i < physicalTrailingBreaks(l, trailingNewlineCount(value)); i++ {
		sb.WriteString(l.lineBreak)
	}
	return sb.String(), true
}

// blockInlineFallback renders a value that block style cannot represent
// (too deep leading spaces, control characters) as a double quoted inline
// scalar while keeping the required separator before following content.
func blockInlineFallback(value string, l blockLayout) string {
	quoted := strconv.Quote(value)
	if l.hasBreak {
		quoted += l.lineBreak
	}
	return quoted
}

// physicalTrailingBreaks returns the number of line breaks emitted after
// the last content line. Chomping removes breaks from the decoded value,
// but a following structure line still needs one physical break as a
// separator.
func physicalTrailingBreaks(l blockLayout, trailing int) int {
	if trailing == 0 {
		if l.hasSibling {
			return 1
		}
		return 0
	}
	return trailing
}

// renderEmptyBlock renders an empty value using single quotes, keeping
// the original same line comment and header break.
func renderEmptyBlock(trailingComment string, l blockLayout) string {
	var sb strings.Builder
	sb.WriteString("''")
	if trailingComment != "" {
		sb.WriteByte(' ')
		sb.WriteString(trailingComment)
	}
	// The original header break already separated the header from the next
	// structure line; the region ended before that next line, so the break
	// is part of neither region nor suffix and must be re-emitted here.
	if l.hasBreak {
		sb.WriteString(l.lineBreak)
	}
	return sb.String()
}

// headerTrailingComment returns the original same line comment of the
// block header, without the leading '#'.
func headerTrailingComment(src string, lines []lineInfo, l blockLayout) string {
	for _, ln := range lines {
		if l.indicatorOff >= ln.start && l.indicatorOff < ln.start+len(ln.text)+len(ln.brk) {
			rest := ln.text[l.headerEndOff-ln.start:]
			rest = strings.TrimLeft(rest, " \t")
			if strings.HasPrefix(rest, "#") {
				return rest
			}
			return ""
		}
	}
	return ""
}

// trailingNewlineCount counts the trailing '\n' characters in value.
func trailingNewlineCount(value string) int {
	n := 0
	for i := len(value); i > 0 && value[i-1] == '\n'; i-- {
		n++
	}
	return n
}
