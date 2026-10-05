package edit

import (
	"strings"

	"github.com/goccy/go-yaml/ast"
)

// lineInfo describes one physical line of the source, excluding its line
// break sequence.
type lineInfo struct {
	// start is the byte offset of the first character of the line.
	start int
	// text is the line content without its trailing line break.
	text string
	// brk is the line break sequence that terminates the line ("" at EOF).
	brk string
}

// splitLines splits src into physical lines, recording byte offsets and
// the original line break sequence ("\n" or "\r\n").
func splitLines(src string) []lineInfo {
	var lines []lineInfo
	start := 0
	for i := 0; i < len(src); {
		if src[i] == '\n' {
			brk := "\n"
			if i > 0 && src[i-1] == '\r' {
				brk = "\r\n"
				lines = append(lines, lineInfo{start: start, text: src[start : i-1], brk: brk})
			} else {
				lines = append(lines, lineInfo{start: start, text: src[start:i], brk: brk})
			}
			i++
			start = i
			continue
		}
		i++
	}
	if start < len(src) {
		lines = append(lines, lineInfo{start: start, text: src[start:], brk: ""})
	}
	return lines
}

// colOffset converts a 1-based byte column on the given line to an
// absolute byte offset. It validates that the column is a single byte
// boundary inside the line text.
func colOffset(lines []lineInfo, line, col int) int {
	if line < 1 || line > len(lines) || col < 1 {
		return -1
	}
	text := lines[line-1].text
	idx := col - 1
	if idx > len(text) {
		return -1
	}
	return lines[line-1].start + idx
}

// replaceQuoted computes the range and replacement for a single or double
// quoted scalar.
func replaceQuoted(src string, lines []lineInfo, tgt *target, value string) (ScalarRange, []byte, error) {
	tk := tgt.node.GetToken()
	start := colOffset(lines, tk.Position.Line, tk.Position.Column)
	if start < 0 {
		return ScalarRange{}, nil, ErrPositionMismatch
	}
	quote := byte('\'')
	if tgt.style == styleDouble {
		quote = '"'
	}
	if start >= len(src) || src[start] != quote {
		return ScalarRange{}, nil, ErrPositionMismatch
	}

	end, ok := scanQuoted(src, start, quote)
	if !ok {
		return ScalarRange{}, nil, ErrPositionMismatch
	}
	lexeme := src[start:end]
	decoded, ok := decodeQuotedLexeme(lexeme)
	if !ok || decoded != tk.Value {
		return ScalarRange{}, nil, ErrValueMismatch
	}

	rng := rangeOf(lines, start, end)
	replacement := renderQuoted(value, tgt.style)
	return rng, []byte(replacement), nil
}

// scanQuoted returns the half-open end offset of the quoted scalar that
// begins at start (the opening quote).
func scanQuoted(src string, start int, quote byte) (int, bool) {
	i := start + 1
	for i < len(src) {
		c := src[i]
		if quote == '\'' && c == '\'' {
			if i+1 < len(src) && src[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1, true
		}
		if quote == '"' && c == '\\' {
			if i+1 >= len(src) {
				return 0, false
			}
			i += 2
			continue
		}
		if c == quote {
			return i + 1, true
		}
		if c == '\n' || c == '\r' {
			return 0, false
		}
		i++
	}
	return 0, false
}

// replacePlain computes the range and replacement for a plain scalar.
func replacePlain(src string, lines []lineInfo, tgt *target, value string) (ScalarRange, []byte, error) {
	tk := tgt.node.GetToken()
	start := colOffset(lines, tk.Position.Line, tk.Position.Column)
	if start < 0 {
		return ScalarRange{}, nil, ErrPositionMismatch
	}

	end, ok := scanPlain(src, lines, start, tk.Position.Column, tgt.flowDepth)
	if !ok {
		return ScalarRange{}, nil, ErrPositionMismatch
	}
	lexeme := src[start:end]
	decoded := normalizePlainLexeme(lexeme)
	if decoded != tk.Value {
		return ScalarRange{}, nil, ErrValueMismatch
	}

	rng := rangeOf(lines, start, end)
	replacement := renderPlain(value, tgt)
	return rng, []byte(replacement), nil
}

// scanPlain finds the half-open end offset of the plain scalar starting at
// start on line with 1-based first column startCol.
func scanPlain(src string, lines []lineInfo, start, startCol, flowDepth int) (int, bool) {
	if start >= len(src) {
		return 0, false
	}
	lineIdx := -1
	for i, ln := range lines {
		if start >= ln.start && start <= ln.start+len(ln.text) {
			lineIdx = i
			break
		}
	}
	if lineIdx < 0 {
		return 0, false
	}

	inFlow := flowDepth > 0
	end := scanPlainLine(src, lines, lineIdx, start, inFlow)
	// Multi line plain scalar: following lines that are more indented than
	// the first column (and not document markers) continue the scalar.
	for next := lineIdx + 1; next < len(lines); next++ {
		ln := lines[next]
		indent := countLeadingSpaces(ln.text)
		if len(ln.text) == indent {
			// Blank line could belong to the scalar, but goccy ends plain
			// scalars at blank lines in block context.
			if inFlow {
				continue
			}
			break
		}
		// Any indented following line continues a plain scalar; a line at
		// column 1 starts a new sibling mapping entry.
		if indent == 0 {
			break
		}
		if isBlockIndicatorAt(ln.text, indent) {
			break
		}
		end = scanPlainLine(src, lines, next, ln.start+indent, inFlow)
	}
	return end, true
}

// scanPlainLine scans one plain scalar line beginning at lineStart.
func scanPlainLine(src string, lines []lineInfo, lineIdx, lineStart int, inFlow bool) int {
	ln := lines[lineIdx]
	i := lineStart
	lineEnd := ln.start + len(ln.text)
	for i < lineEnd {
		c := src[i]
		// A comment starts at '#' preceded by whitespace or line start.
		if c == '#' && (i == lineStart || src[i-1] == ' ' || src[i-1] == '\t') {
			break
		}
		if c == ':' {
			if i+1 >= lineEnd || src[i+1] == ' ' || src[i+1] == '\t' {
				if !inFlow {
					break
				}
			}
			if inFlow && (i+1 >= lineEnd || src[i+1] == ',' || src[i+1] == ']' || src[i+1] == '}') {
				break
			}
		}
		if inFlow && (c == ',' || c == ']' || c == '}') {
			break
		}
		i++
	}
	// Trim trailing spaces/tabs of the scalar on this line.
	for i > lineStart && (src[i-1] == ' ' || src[i-1] == '\t') {
		i--
	}
	return i
}

func countLeadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

func isBlockIndicatorAt(s string, idx int) bool {
	if idx < 0 || idx >= len(s) {
		return false
	}
	if s[idx] != '-' && s[idx] != '?' && s[idx] != ':' {
		return false
	}
	return idx+1 >= len(s) || s[idx+1] == ' ' || s[idx+1] == '\t'
}

func isDocumentMarker(s string) bool {
	t := strings.TrimRight(s, " \t")
	return t == "---" || t == "..."
}

// normalizePlainLexeme folds line breaks and surrounding indentation into
// single spaces the way YAML plain scalars do.
func normalizePlainLexeme(lexeme string) string {
	if !strings.ContainsAny(lexeme, "\n\r ") {
		return lexeme
	}
	var sb strings.Builder
	lines := strings.FieldsFunc(lexeme, func(r rune) bool { return r == '\n' || r == '\r' })
	first := true
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if !first {
			sb.WriteByte(' ')
		}
		sb.WriteString(trimmed)
		first = false
	}
	return sb.String()
}

// rangeOf builds a ScalarRange from absolute byte offsets.
func rangeOf(lines []lineInfo, start, end int) ScalarRange {
	rng := ScalarRange{StartOffset: start, EndOffset: end}
	for i, ln := range lines {
		lineNo := i + 1
		lineEnd := ln.start + len(ln.text)
		if start >= ln.start && start <= lineEnd {
			rng.StartLine = lineNo
			rng.StartColumn = start - ln.start + 1
		}
		if end > ln.start && end <= lineEnd {
			rng.EndLine = lineNo
			rng.EndColumn = end - ln.start
		}
	}
	if rng.EndLine == 0 {
		for i := len(lines) - 1; i >= 0; i-- {
			if end >= lines[i].start {
				rng.EndLine = i + 1
				rng.EndColumn = len(lines[i].text)
				break
			}
		}
	}
	return rng
}

// walkScalars visits every scalar leaf node in the file.
func walkScalars(file *ast.File, fn func(ast.Node)) {
	for _, doc := range file.Docs {
		walkNode(doc, fn)
	}
}

func walkNode(n ast.Node, fn func(ast.Node)) {
	if n == nil {
		return
	}
	switch v := n.(type) {
	case *ast.DocumentNode:
		walkNode(v.Body, fn)
	case *ast.MappingNode:
		for _, mv := range v.Values {
			walkNode(mv, fn)
		}
	case *ast.MappingValueNode:
		walkNode(v.Key, fn)
		walkNode(v.Value, fn)
	case *ast.SequenceNode:
		for _, e := range v.Values {
			walkNode(e, fn)
		}
	case *ast.SequenceEntryNode:
		walkNode(v.Value, fn)
	case *ast.LiteralNode:
		fn(v)
	case *ast.AnchorNode:
		walkNode(v.Value, fn)
	case *ast.TagNode:
		walkNode(v.Value, fn)
	default:
		if _, ok := nodeStyle(v); ok {
			fn(v)
		}
	}
}
