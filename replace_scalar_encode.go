package yaml

import (
	"fmt"
	"strings"
)

// encodeCandidates returns replacement byte sequences in preference order.
// The first candidate that re-parses to exactly newValue without disturbing
// any other node is used; the double-quoted candidate always succeeds.
func encodeCandidates(source []byte, span scalarSpan, oldStyle scalarStyle, value string) [][]byte {
	plain := []byte(value)
	single, singleOK := encodeSingleQuoted(value)
	double := []byte(encodeDoubleQuoted(value))

	var ordered []scalarStyle
	switch oldStyle {
	case stylePlain:
		ordered = []scalarStyle{stylePlain, styleSingleQuote, styleDoubleQuote}
	case styleSingleQuote:
		ordered = []scalarStyle{styleSingleQuote, styleDoubleQuote, stylePlain}
	case styleDoubleQuote:
		ordered = []scalarStyle{styleDoubleQuote, styleSingleQuote, stylePlain}
	case styleLiteral:
		ordered = []scalarStyle{styleLiteral, styleFolded, styleSingleQuote, stylePlain, styleDoubleQuote}
	case styleFolded:
		ordered = []scalarStyle{styleFolded, styleLiteral, styleSingleQuote, stylePlain, styleDoubleQuote}
	}

	var out [][]byte
	seen := map[string]bool{}
	add := func(b []byte) {
		if b == nil {
			return
		}
		key := string(b)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, b)
	}
	// When the replaced span is a block scalar ending with a line break, a
	// non-block replacement must terminate with that same break (and any
	// comment that followed the block header has to be dropped, since it now
	// would attach to a different line).
	tail := []byte{}
	if span.targetIsBlock() && endsWithLineBreak(source, span) {
		nl := span.nl
		if nl == "" {
			nl = "\n"
		}
		if span.headerComment != "" {
			tail = []byte(" " + span.headerComment + nl)
		} else {
			tail = []byte(nl)
		}
	}
	for _, st := range ordered {
		switch st {
		case stylePlain:
			add(append(append([]byte{}, plain...), tail...))
		case styleSingleQuote:
			if singleOK {
				add(append(append([]byte{}, single...), tail...))
			}
		case styleDoubleQuote:
			add(append(append([]byte{}, double...), tail...))
		default:
			if enc, ok := encodeBlock(source, span, blockKind(st), value); ok {
				add([]byte(enc))
			}
		}
	}
	return out
}

func blockKind(st scalarStyle) byte {
	if st == styleFolded {
		return '>'
	}
	return '|'
}

// encodeSingleQuoted returns a single-quoted scalar. Single-quoted scalars
// cannot encode line breaks other than folded line continuations, so values
// containing line breaks are rejected here.
func encodeSingleQuoted(value string) ([]byte, bool) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, false
	}
	s := "'" + strings.ReplaceAll(value, "'", "''") + "'"
	return []byte(s), true
}

// encodeDoubleQuoted returns a double-quoted scalar that can represent any
// string without information loss.
func encodeDoubleQuoted(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\0`)
		case 7:
			b.WriteString(`\a`)
		case 8:
			b.WriteString(`\b`)
		case 11:
			b.WriteString(`\v`)
		case 12:
			b.WriteString(`\f`)
		case 27:
			b.WriteString(`\e`)
		default:
			if r < 0x20 || r == 0x7f || r == 0xFEFF {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// encodeBlock renders value as a block scalar (literal or folded) using the
// position metadata of the replaced scalar. It returns ok=false for values
// that cannot be represented as a block scalar here.
func encodeBlock(source []byte, span scalarSpan, kind byte, value string) (string, bool) {
	nl := span.nl
	if nl == "" {
		nl = "\n"
	}

	// Block scalars normalize line breaks and cannot carry a bare '\r'.
	if strings.ContainsRune(value, '\r') {
		return "", false
	}

	trailing := 0
	for trailing < len(value) && value[len(value)-1-trailing] == '\n' {
		trailing++
	}
	core := value[:len(value)-trailing]

	var chomp byte
	switch {
	case trailing == 0:
		chomp = '-'
	case trailing == 1:
		chomp = 0
	default:
		chomp = '+'
	}

	parent := span.parentIndent
	base := parent + 1
	if !span.empty {
		base = span.contentIndent
	}
	if base < 1 {
		base = 1
	}

	var header, body string

	if core == "" {
		// No content lines.
		if value == "" {
			// Empty value: a clip header with no content lines.
			chomp = 0
		} else {
			// Only newlines: keep with one physical line per newline.
			chomp = '+'
		}
		header = blockHeader(kind, base-parent, base-parent > 0, chomp, span.headerComment, nl)
		if value == "" {
			return header, true
		}
		pad := strings.Repeat(" ", base)
		return header + pad + strings.Repeat(nl, trailing), true
	}

	// Leading newlines cannot be expressed in a block scalar.
	if core[0] == '\n' {
		return "", false
	}

	lines := strings.Split(core, "\n")

	indent := base
	explicit := false
	digit := 0
	leadFirst := leadingSpaces(lines[0])
	if lines[0] == "" || leadFirst > 0 {
		explicit = true
		if lines[0] != "" {
			// The first line carries its own leading spaces on top of the
			// content indentation, so padding stays at base and the explicit
			// digit records the absolute content indentation.
			indent = base
			digit = base
		} else {
			digit = base - parent
			if digit < 1 {
				digit = 1
			}
			indent = parent + digit
		}
		if digit < 1 || digit > 9 {
			return "", false
		}
	}

	pad := strings.Repeat(" ", indent)
	for i, line := range lines {
		if i > 0 {
			body += nl
		}
		if line != "" {
			body += pad + line
		}
	}

	// The final content line keeps its line break whenever the replaced span
	// owned one; keep chomping adds the remaining blank lines.
	if endsWithLineBreak(source, span) {
		body += nl
		if chomp == '+' {
			body += strings.Repeat(nl, trailing-1)
		}
	} else {
		if chomp == '+' {
			body += strings.Repeat(nl, trailing)
		}
		// clip/strip at EOF without a line break: nothing appended.
	}

	header = blockHeader(kind, digit, explicit, chomp, span.headerComment, nl)
	return header + body, true
}

func blockHeader(kind byte, digit int, explicit bool, chomp byte, comment, nl string) string {
	h := string(kind)
	if explicit {
		h += string(rune('0' + digit))
	}
	if chomp != 0 {
		h += string(chomp)
	}
	if comment != "" {
		h += " " + comment
	}
	return h + nl
}

func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}
