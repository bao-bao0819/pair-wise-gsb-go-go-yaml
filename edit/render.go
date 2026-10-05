package edit

import (
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/token"
)

// decodeQuotedLexeme decodes a complete single or double quoted lexeme
// including the surrounding quotes.
func decodeQuotedLexeme(lexeme string) (string, bool) {
	if len(lexeme) < 2 {
		return "", false
	}
	switch lexeme[0] {
	case '\'':
		if lexeme[len(lexeme)-1] != '\'' {
			return "", false
		}
		inner := lexeme[1 : len(lexeme)-1]
		return strings.ReplaceAll(inner, "''", "'"), true
	case '"':
		v, err := strconv.Unquote(lexeme)
		if err != nil {
			return "", false
		}
		return v, true
	}
	return "", false
}

// renderQuoted renders value, preferring the original quoting style and
// switching to a style that can represent it when required.
func renderQuoted(value string, style scalarStyle) string {
	switch style {
	case styleSingle:
		if canSingleQuote(value) {
			return "'" + strings.ReplaceAll(value, "'", "''") + "'"
		}
	case styleDouble:
		// Double quoting can represent every string.
	}
	return strconv.Quote(value)
}

// canSingleQuote reports whether value is representable in a single quoted
// scalar. Single quotes can contain any character except control
// characters that YAML forbids; line breaks inside require multi line
// quoting which we avoid by falling back.
func canSingleQuote(value string) bool {
	if strings.ContainsAny(value, "\n\r") {
		return false
	}
	for _, r := range value {
		if r < 0x20 && r != '\t' {
			return false
		}
	}
	return true
}

// renderPlain renders value trying to stay a plain scalar. For a typed
// scalar (int, bool, null, ...) the new value is kept plain only when it
// parses to the same kind; for a string scalar it stays plain when it
// decodes to itself as a string.
func renderPlain(value string, tgt *target) string {
	switch tgt.node.GetToken().Type {
	case token.StringType:
		if canBePlainString(value) {
			return value
		}
	default:
		if canBePlainTyped(value, tgt.node.GetToken().Type) {
			return value
		}
		// The node was typed (e.g. integer); replacing it with a string
		// that looks like plain text requires quoting to stay a string.
	}
	if canSingleQuote(value) {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return strconv.Quote(value)
}

// canBePlainString reports whether value is a single line plain scalar
// decoding to exactly value as a string.
func canBePlainString(value string) bool {
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, "\n\r\t") {
		return false
	}
	return !token.IsNeedQuoted(value)
}

// canBePlainTyped reports whether replacing a typed scalar keeps the same
// lexical kind.
func canBePlainTyped(value string, kind token.Type) bool {
	if value == "" || strings.ContainsAny(value, "\n\r\t") {
		return false
	}
	if token.IsNeedQuoted(value) {
		// Reserved keywords such as null/true are "need quoted" when used
		// as a string, but for a typed scalar that is exactly the point.
		if !isReservedOfKind(value, kind) {
			return false
		}
	}
	switch kind {
	case token.BoolType, token.NullType, token.InfinityType, token.NanType:
		return tokenKind(value) == kind
	case token.IntegerType, token.BinaryIntegerType, token.OctetIntegerType,
		token.HexIntegerType, token.FloatType:
		return isNumberKind(value, kind)
	}
	return false
}

// tokenKind classifies a reserved keyword token type.
func tokenKind(value string) token.Type {
	tk := token.New(value, value, nil)
	return tk.Type
}

func isReservedOfKind(value string, kind token.Type) bool {
	return tokenKind(value) == kind
}

// isNumberKind checks numeric compatibility with the target kind, where
// all integer sub-kinds are interchangeable as plain integers.
func isNumberKind(value string, kind token.Type) bool {
	tk := tokenKind(value)
	switch kind {
	case token.FloatType:
		return tk == token.FloatType
	case token.IntegerType, token.BinaryIntegerType,
		token.OctetIntegerType, token.HexIntegerType:
		switch tk {
		case token.IntegerType, token.BinaryIntegerType,
			token.OctetIntegerType, token.HexIntegerType:
			return true
		}
	}
	return false
}
