// Package edit provides surgical, position based edits of YAML documents.
//
// Unlike re-encoding a whole document, the functions in this package only
// touch the byte range that belongs to the targeted node. Comments,
// indentation, line breaks (including CRLF), document markers and every
// other byte are preserved verbatim.
package edit

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/lexer"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

var (
	// ErrNodeNotFound is returned when the provided node cannot be located
	// in a fresh parse of the source.
	ErrNodeNotFound = fmt.Errorf("yaml/edit: node not found in source")
	// ErrUnsupportedNode is returned when the targeted node is not an
	// editable scalar node.
	ErrUnsupportedNode = fmt.Errorf("yaml/edit: unsupported node")
	// ErrPositionMismatch is returned when the source bytes at the node
	// position do not match the node, so the edit is refused.
	ErrPositionMismatch = fmt.Errorf("yaml/edit: node position does not match source")
	// ErrValueMismatch is returned when the scalar currently stored at the
	// node position is not equal to the value held by the node.
	ErrValueMismatch = fmt.Errorf("yaml/edit: current scalar value does not match node value")
)

// ScalarRange describes the byte range of a scalar in the original source.
//
// StartLine, StartColumn, EndLine and EndColumn are 1-based byte columns
// reported at the first and last replaced byte. StartOffset and EndOffset
// are the half-open byte interval [StartOffset, EndOffset) in src.
type ScalarRange struct {
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
	StartOffset int
	EndOffset   int
}

func (r ScalarRange) String() string {
	return fmt.Sprintf(
		"[%d:%d..%d:%d] (bytes [%d,%d))",
		r.StartLine, r.StartColumn, r.EndLine, r.EndColumn, r.StartOffset, r.EndOffset,
	)
}

// scalarStyle is the lexical style of a scalar node.
type scalarStyle int

const (
	stylePlain scalarStyle = iota
	styleSingle
	styleDouble
	styleLiteral
	styleFolded
)

// target describes the scalar node to replace, located in a fresh parse
// of the source that corresponds to src.
type target struct {
	node  ast.Node
	style scalarStyle
	line  int
	col   int
	// flowDepth is the depth of flow collections enclosing the scalar.
	flowDepth int
	// For block scalars, header holds the Literal/Folded token.
	header *token.Token
}

// flowDepthBefore counts unclosed flow collection indicators at and
// before keyTok in the token stream.
func flowDepthBefore(tokens token.Tokens, keyTok *token.Token) int {
	depth := 0
	for _, tk := range tokens {
		if tk == keyTok {
			break
		}
		switch tk.Type {
		case token.SequenceStartType, token.MappingStartType:
			depth++
		case token.SequenceEndType, token.MappingEndType:
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

// ReplaceScalar replaces the scalar identified by node with value in src.
//
// src must be the exact source text that node was parsed from. The node is
// re-located in a fresh parse of src and the edit is rejected unless the
// bytes at its position still represent the node and its value.
//
// The returned text keeps the original style whenever value can be
// represented losslessly in it; otherwise a quoting or block style that
// represents value exactly is used. Block scalar chomping and indentation
// indicators are derived from the real trailing line breaks and leading
// spaces of value, and folded style never folds line breaks into spaces.
// Every byte outside the scalar lexical range, including CRLF line
// breaks, same line comments and document separators, is preserved.
func ReplaceScalar(src []byte, node ast.Node, value string) ([]byte, ScalarRange, error) {
	rng, replacement, tgt, err := planReplace(string(src), node, value)
	if err != nil {
		return nil, ScalarRange{}, err
	}
	out := make([]byte, 0, len(src)-len(src[rng.StartOffset:rng.EndOffset])+len(replacement))
	out = append(out, src[:rng.StartOffset]...)
	out = append(out, replacement...)
	out = append(out, src[rng.EndOffset:]...)

	if err := verifyReplacement(
		string(src), string(out),
		node.GetPath(), tgt.line, tgt.col, tgt.node.GetToken().Type.String(),
		value,
	); err != nil {
		return nil, ScalarRange{}, err
	}
	return out, rng, nil
}

// planReplace locates the target, computes its lexical range and renders
// the replacement text.
func planReplace(src string, node ast.Node, value string) (ScalarRange, []byte, *target, error) {
	if node == nil || node.GetToken() == nil || node.GetToken().Position == nil {
		return ScalarRange{}, nil, nil, ErrUnsupportedNode
	}
	tokens := lexer.Tokenize(src)
	file, err := parser.Parse(tokens, parser.ParseComments)
	if err != nil {
		return ScalarRange{}, nil, nil, fmt.Errorf("yaml/edit: source parse error: %w", err)
	}

	tgt, err := locateTarget(file, tokens, node)
	if err != nil {
		return ScalarRange{}, nil, nil, err
	}
	// Even when line/column/path coincide, refuse the edit if the scalar
	// value currently decoded from src differs from the node the caller
	// holds. This prevents writing at a stale position.
	if caller, fresh := scalarIdentity(node), scalarIdentity(tgt.node); caller != fresh {
		return ScalarRange{}, nil, nil, ErrValueMismatch
	}

	lines := splitLines(src)
	var rng ScalarRange
	var replacement []byte
	switch tgt.style {
	case styleLiteral, styleFolded:
		rng, replacement, err = replaceBlock(src, lines, tgt, value)
	case styleSingle, styleDouble:
		rng, replacement, err = replaceQuoted(src, lines, tgt, value)
	default:
		rng, replacement, err = replacePlain(src, lines, tgt, value)
	}
	if err != nil {
		return ScalarRange{}, nil, nil, err
	}
	return rng, replacement, tgt, nil
}

// scalarIdentity returns a stable identity of a scalar node combining its
// style and decoded value.
func scalarIdentity(n ast.Node) string {
	style, _ := nodeStyle(n)
	return string(rune('a'+style)) + ":" + nodeDecodedValue(n)
}

// locateTarget re-parses src and finds the scalar node that corresponds
// to the caller provided node. Matching uses line/column coordinates and
// the YAML path; the reported offset is never trusted because it can drift
// around block scalars.
func locateTarget(file *ast.File, tokens token.Tokens, node ast.Node) (*target, error) {
	wantLine := node.GetToken().Position.Line
	wantCol := node.GetToken().Position.Column
	wantPath := node.GetPath()

	var found *target
	walkScalars(file, func(n ast.Node) {
		if found != nil {
			return
		}
		tk := n.GetToken()
		if tk == nil || tk.Position == nil {
			return
		}
		if tk.Position.Line != wantLine || tk.Position.Column != wantCol {
			return
		}
		if wantPath != "" && n.GetPath() != wantPath {
			return
		}
		// Keys and values can share a path. Tie break using the token type
		// and, when on the same coordinates is impossible, the token value
		// kind. Scalar leaf nodes at the same line/col cannot coexist, but
		// guard against matching the mapping key token anyway.
		if !sameTokenKind(n, node) {
			return
		}
		style, ok := nodeStyle(n)
		if !ok {
			return
		}
		t := &target{
			node:  n,
			style: style,
			line:  wantLine,
			col:   wantCol,
		}
		var keyTok *token.Token
		if lit, isBlock := n.(*ast.LiteralNode); isBlock {
			t.header = lit.Start
			keyTok = t.header
		} else {
			keyTok = tk
		}
		t.flowDepth = flowDepthBefore(tokens, keyTok)
		found = t
	})
	if found == nil {
		return nil, ErrNodeNotFound
	}
	return found, nil
}

// sameTokenKind reports whether two scalar nodes refer to the same kind
// of token, preventing a key matching a value with an identical path.
func sameTokenKind(a, b ast.Node) bool {
	ak, aok := scalarKindKey(a)
	bk, bok := scalarKindKey(b)
	return aok && bok && ak == bk
}

func scalarKindKey(n ast.Node) (string, bool) {
	style, ok := nodeStyle(n)
	if !ok {
		return "", false
	}
	tk := n.GetToken()
	return string(rune(style)) + ":" + tk.Type.String() + ":" + tk.Value, true
}

// nodeDecodedValue returns the decoded value of a scalar leaf node.
func nodeDecodedValue(n ast.Node) string {
	if lit, ok := n.(*ast.LiteralNode); ok {
		if lit.Value != nil {
			return lit.Value.Value
		}
		return ""
	}
	if s, ok := n.(*ast.StringNode); ok {
		return s.Value
	}
	return n.GetToken().Value
}

// nodeStyle reports the lexical style of a scalar leaf node.
func nodeStyle(n ast.Node) (scalarStyle, bool) {
	if lit, ok := n.(*ast.LiteralNode); ok {
		if lit.Start == nil {
			return 0, false
		}
		switch lit.Start.Type {
		case token.LiteralType:
			return styleLiteral, true
		case token.FoldedType:
			return styleFolded, true
		}
		return 0, false
	}
	switch n.(type) {
	case *ast.StringNode,
		*ast.NullNode,
		*ast.BoolNode,
		*ast.IntegerNode,
		*ast.FloatNode,
		*ast.InfinityNode,
		*ast.NanNode:
	default:
		return 0, false
	}
	tk := n.GetToken()
	switch tk.Type {
	case token.SingleQuoteType:
		return styleSingle, true
	case token.DoubleQuoteType:
		return styleDouble, true
	case token.StringType,
		token.NullType,
		token.ImplicitNullType,
		token.BoolType,
		token.IntegerType,
		token.BinaryIntegerType,
		token.OctetIntegerType,
		token.HexIntegerType,
		token.FloatType,
		token.InfinityType,
		token.NanType:
		return stylePlain, true
	}
	return 0, false
}
