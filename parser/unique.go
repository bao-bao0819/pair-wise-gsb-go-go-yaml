package parser

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/internal/errors"
	"github.com/goccy/go-yaml/token"
)

// UniqueMapKeyError is returned when a mapping declares two keys that are
// semantically equal under the YAML scalar resolution rules (and therefore
// cannot coexist in the same mapping).
//
// Unlike a literal text comparison, keys are compared after resolving their
// scalar types, so e.g. "0x10" and "16" are equal, but "1" (string) and 1
// (integer) are not.
//
// Token points at the duplicate declaration and FirstToken points at the first
// declaration of the same key.
type UniqueMapKeyError = errors.UniqueMapKeyError

// uniqueMapKeyKind is the resolved equivalence class of a mapping key.
// Keys are equal only when both their kind and their value match.
type uniqueMapKeyKind int

const (
	uniqueKeyNull uniqueMapKeyKind = iota
	uniqueKeyBool
	uniqueKeyInt
	uniqueKeyFloat
	uniqueKeyString
	// uniqueKeyTagged is the equivalence class for a scalar carrying an
	// explicit tag that is not one of the core resolved tags
	// (e.g. !!binary, !!timestamp or a custom tag). Keys are compared by the
	// tag handle together with the resolved scalar text.
	uniqueKeyTagged
)

// uniqueMapKey is a comparable identity for a mapping key.
//
// It deliberately does not rely on Go's map key semantics for float or
// interface values: comparison is performed explicitly by equalUniqueMapKey
// so that the rules for NaN and negative zero stay documented and predictable.
type uniqueMapKey struct {
	kind         uniqueMapKeyKind
	boolValue    bool
	intValue     *big.Int
	floatValue   float64
	stringValue  string
	tagText      string
	tagTextValue string
}

type seenMapKey struct {
	key   uniqueMapKey
	token *token.Token
}

func equalUniqueMapKey(a, b uniqueMapKey) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case uniqueKeyNull:
		return true
	case uniqueKeyBool:
		return a.boolValue == b.boolValue
	case uniqueKeyInt:
		return a.intValue.Cmp(b.intValue) == 0
	case uniqueKeyFloat:
		// Follow IEEE 754 / Go == semantics explicitly:
		// NaN is equal to nothing, including another NaN, so multiple
		// ".nan" keys are never reported as duplicates.
		if math.IsNaN(a.floatValue) || math.IsNaN(b.floatValue) {
			return false
		}
		return a.floatValue == b.floatValue
	case uniqueKeyString:
		return a.stringValue == b.stringValue
	case uniqueKeyTagged:
		return a.tagText == b.tagText && a.tagTextValue == b.tagTextValue
	}
	return false
}

// validateUniqueMapKeys reports semantically duplicated mapping keys in the
// parsed file. Positions are taken from the AST tokens; the source YAML is
// never re-serialized to locate a key.
//
// Merge keys ("<<") are skipped here: overrides introduced by a merge follow
// the existing merge rules and are reported separately from explicit
// duplicate declarations within the same mapping.
func validateUniqueMapKeys(file *ast.File) error {
	for _, doc := range file.Docs {
		if err := validateUniqueMapKeysNode(doc.Body); err != nil {
			return err
		}
	}
	return nil
}

func validateUniqueMapKeysNode(node ast.Node) error {
	switch n := node.(type) {
	case *ast.DocumentNode:
		return validateUniqueMapKeysNode(n.Body)
	case *ast.MappingNode:
		seen := make([]seenMapKey, 0, len(n.Values))
		for _, entry := range n.Values {
			keyNode := entry.Key
			if keyNode == nil {
				continue
			}
			if keyNode.IsMergeKey() {
				// Merge-introduced overrides are governed by the merge
				// rules, not the explicit duplicate declaration rule.
				if entry.Value != nil {
					if err := validateUniqueMapKeysNode(entry.Value); err != nil {
						return err
					}
				}
				continue
			}
			key, comparable := resolveUniqueMapKey(keyNode)
			if comparable {
				for _, previous := range seen {
					if equalUniqueMapKey(previous.key, key) {
						return errors.ErrUniqueMapKey(
							formatUniqueMapKeyMessage(keyNode, previous.token),
							keyNode.GetToken(),
							previous.token,
						)
					}
				}
				seen = append(seen, seenMapKey{key: key, token: keyNode.GetToken()})
			}
			// Recurse into complex keys and into the value.
			if complex := complexKeyNode(keyNode); complex != nil {
				if err := validateUniqueMapKeysNode(complex); err != nil {
					return err
				}
			}
			if entry.Value != nil {
				if err := validateUniqueMapKeysNode(entry.Value); err != nil {
					return err
				}
			}
		}
	case *ast.SequenceNode:
		for _, value := range n.Values {
			if err := validateUniqueMapKeysNode(value); err != nil {
				return err
			}
		}
	case *ast.TagNode:
		if n.Value != nil {
			return validateUniqueMapKeysNode(n.Value)
		}
	case *ast.AnchorNode:
		if n.Value != nil {
			return validateUniqueMapKeysNode(n.Value)
		}
	}
	return nil
}

func complexKeyNode(node ast.MapKeyNode) ast.Node {
	if mappingKey, ok := node.(*ast.MappingKeyNode); ok {
		switch mappingKey.Value.(type) {
		case *ast.MappingNode, *ast.SequenceNode:
			return mappingKey.Value
		}
	}
	return nil
}

func formatUniqueMapKeyMessage(duplicate ast.MapKeyNode, firstToken *token.Token) string {
	keyText := uniqueMapKeyText(duplicate)
	if firstToken != nil && firstToken.Position != nil {
		return "mapping key " + strconv.Quote(keyText) + " already defined at [" +
			strconv.Itoa(firstToken.Position.Line) + ":" + strconv.Itoa(firstToken.Position.Column) + "]"
	}
	return "duplicate mapping key " + strconv.Quote(keyText)
}

// uniqueMapKeyText keeps the original spelling of the key as it appeared in
// the source, including quotes, integer bases and tag prefixes.
func uniqueMapKeyText(node ast.Node) string {
	switch n := node.(type) {
	case *ast.MappingKeyNode:
		if n.Value == nil {
			return ""
		}
		return uniqueMapKeyText(n.Value)
	case *ast.AnchorNode:
		return n.String()
	case *ast.TagNode:
		return n.String()
	case *ast.AliasNode:
		return n.String()
	}
	tk := node.GetToken()
	if tk == nil {
		return node.String()
	}
	if strings.TrimSpace(tk.Origin) != "" {
		return strings.TrimSpace(tk.Origin)
	}
	return tk.Value
}

// resolveUniqueMapKey resolves a key node into a comparable identity.
// The second return value reports whether the key participates in uniqueness
// checking: complex keys (mappings, sequences) and alias keys are not
// comparable and are skipped with an explicit rule rather than relying on any
// accidental Go map behavior.
func resolveUniqueMapKey(node ast.MapKeyNode) (uniqueMapKey, bool) {
	// Unwrap structural wrappers; anchors and explicit "?" keys do not change
	// the key identity.
	switch n := node.(type) {
	case *ast.MappingKeyNode:
		if n.Value == nil {
			return uniqueMapKey{}, false
		}
		inner, ok := n.Value.(ast.MapKeyNode)
		if !ok {
			// a complex key (mapping/sequence)
			return uniqueMapKey{}, false
		}
		return resolveUniqueMapKey(inner)
	case *ast.AnchorNode:
		if n.Value == nil {
			return uniqueMapKey{}, false
		}
		inner, ok := n.Value.(ast.MapKeyNode)
		if !ok {
			return uniqueMapKey{}, false
		}
		return resolveUniqueMapKey(inner)
	case *ast.AliasNode:
		// Aliases can only be compared after anchor resolution, which is not
		// available during syntax validation.
		return uniqueMapKey{}, false
	case *ast.TagNode:
		return resolveTaggedUniqueMapKey(n)
	case *ast.NullNode:
		return uniqueMapKey{kind: uniqueKeyNull}, true
	case *ast.BoolNode:
		return uniqueMapKey{kind: uniqueKeyBool, boolValue: n.Value}, true
	case *ast.IntegerNode:
		return resolveIntUniqueMapKey(scalarLexemeFromNode(n), n.Value)
	case *ast.FloatNode:
		return uniqueMapKey{kind: uniqueKeyFloat, floatValue: n.Value}, true
	case *ast.InfinityNode:
		return uniqueMapKey{kind: uniqueKeyFloat, floatValue: n.Value}, true
	case *ast.NanNode:
		return uniqueMapKey{kind: uniqueKeyFloat, floatValue: math.NaN()}, true
	case *ast.StringNode:
		return uniqueMapKey{kind: uniqueKeyString, stringValue: n.Value}, true
	}
	// LiteralNode and any other non core-resolved scalar do not participate.
	return uniqueMapKey{}, false
}

func resolveTaggedUniqueMapKey(tag *ast.TagNode) (uniqueMapKey, bool) {
	if tag.Value == nil {
		return uniqueMapKey{}, false
	}
	scalar, ok := tag.Value.(ast.ScalarNode)
	if !ok {
		// a tag applied to a complex value cannot be a scalar key
		return uniqueMapKey{}, false
	}
	text := scalarLexemeFromNode(scalar)
	switch token.ReservedTagKeyword(tag.Start.Value) {
	case token.StringTag:
		return uniqueMapKey{kind: uniqueKeyString, stringValue: text}, true
	case token.BooleanTag:
		value, err := strconv.ParseBool(text)
		if err != nil {
			return uniqueMapKey{}, false
		}
		return uniqueMapKey{kind: uniqueKeyBool, boolValue: value}, true
	case token.IntegerTag:
		return resolveIntUniqueMapKey(text, scalar.GetValue())
	case token.FloatTag:
		value, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
		if err != nil {
			return uniqueMapKey{}, false
		}
		return uniqueMapKey{kind: uniqueKeyFloat, floatValue: value}, true
	case token.NullTag:
		return uniqueMapKey{kind: uniqueKeyNull}, true
	}
	// !!binary, !!timestamp, !!merge and custom/local tags are compared by
	// the tag handle plus the resolved scalar text; they never equal a key of
	// a different tag or an untagged scalar.
	return uniqueMapKey{
		kind:         uniqueKeyTagged,
		tagText:      tag.Start.Value,
		tagTextValue: text,
	}, true
}

// scalarLexemeFromNode returns the resolved scalar text of a scalar node,
// which for quoted strings is the unescaped content and for other scalars is
// the token value produced by the tokenizer.
func scalarLexemeFromNode(node ast.Node) string {
	switch n := node.(type) {
	case *ast.StringNode:
		return n.Value
	case *ast.TagNode:
		if n.Value != nil {
			return scalarLexemeFromNode(n.Value)
		}
	}
	if scalar, ok := node.(ast.ScalarNode); ok {
		if value := scalar.GetToken().Value; value != "" {
			return value
		}
	}
	return node.GetToken().Value
}

func resolveIntUniqueMapKey(lexeme string, parsed any) (uniqueMapKey, bool) {
	switch value := parsed.(type) {
	case int64:
		return uniqueMapKey{kind: uniqueKeyInt, intValue: big.NewInt(value)}, true
	case uint64:
		return uniqueMapKey{kind: uniqueKeyInt, intValue: new(big.Int).SetUint64(value)}, true
	}
	// The tokenizer leaves out-of-range integers unparsed; parse the lexeme
	// exactly so that values beyond int64/uint64 keep a defined identity.
	if value, ok := parseBigIntLexeme(lexeme); ok {
		return uniqueMapKey{kind: uniqueKeyInt, intValue: value}, true
	}
	return uniqueMapKey{}, false
}

// parseBigIntLexeme parses a decimal/hexadecimal/octal/binary integer lexeme
// using the same base conventions as scalar resolution.
func parseBigIntLexeme(lexeme string) (*big.Int, bool) {
	normalized := strings.ReplaceAll(lexeme, "_", "")
	negative := false
	switch {
	case strings.HasPrefix(normalized, "-"):
		negative = true
		normalized = normalized[1:]
	case strings.HasPrefix(normalized, "+"):
		normalized = normalized[1:]
	}
	base := 10
	switch {
	case strings.HasPrefix(normalized, "0x") || strings.HasPrefix(normalized, "0X"):
		normalized = normalized[2:]
		base = 16
	case strings.HasPrefix(normalized, "0o") || strings.HasPrefix(normalized, "0O"):
		normalized = normalized[2:]
		base = 8
	case strings.HasPrefix(normalized, "0b") || strings.HasPrefix(normalized, "0B"):
		normalized = normalized[2:]
		base = 2
	case len(normalized) > 1 && normalized[0] == '0':
		base = 8
	}
	value, ok := new(big.Int).SetString(normalized, base)
	if !ok {
		return nil, false
	}
	if negative {
		value.Neg(value)
	}
	return value, true
}
