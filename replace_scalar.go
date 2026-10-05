package yaml

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/internal/errors"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// ScalarRange is the byte-offset range [Start, End) occupied by a scalar
// inside the YAML source it was produced against.
type ScalarRange struct {
	// Start is the byte offset of the first scalar byte.
	Start int
	// End is one past the last scalar byte.
	End int
}

var (
	// ErrScalarNodeRequired is returned when the given node is not a scalar node.
	ErrScalarNodeRequired = errors.New("node is not a scalar node")
	// ErrScalarPositionMismatch is returned when the node position does not
	// match the given source bytes; the caller must re-parse the source before
	// attempting a replacement.
	ErrScalarPositionMismatch = errors.New("scalar node position does not match source")
)

type scalarStyle int

const (
	stylePlain scalarStyle = iota
	styleSingleQuote
	styleDoubleQuote
	styleLiteral
	styleFolded
)

// ReplaceScalar replaces the scalar value identified by node inside source
// with newValue. Only the lexical bytes of the target scalar change; every
// other byte (indentation, comments, document markers, line endings such as
// CRLF) is preserved.
//
// The original scalar style is reused when newValue is representable in it
// without information loss; otherwise a style that can represent it is
// selected. Plain, single-quoted, double-quoted, literal (|) and folded (>)
// scalars are supported. Block indentation and chomping indicators are
// derived from the real trailing newlines of newValue, and a folded scalar
// is never emitted in a way that folds the newlines of newValue into spaces.
//
// The returned ScalarRange is the range [Start, End) of the replacement
// inside the returned text. node must originate from source: a node whose
// recorded position no longer matches source is rejected.
func ReplaceScalar(source []byte, node ast.Node, newValue string) ([]byte, ScalarRange, error) {
	start, err := nodeStart(node, source)
	if err != nil {
		return nil, ScalarRange{}, err
	}

	oldFile, err := parser.ParseBytes(source, parser.ParseComments)
	if err != nil {
		return nil, ScalarRange{}, fmt.Errorf("failed to parse source: %w", err)
	}

	oldEntries, style, err := locateOldScalar(oldFile, start, node)
	if err != nil {
		return nil, ScalarRange{}, err
	}

	var span scalarSpan
	if isImplicitNullNode(node) {
		span, err = scanImplicitNullSpan(source, start)
	} else {
		span, err = scanScalarSpan(source, style, start)
	}
	if err != nil {
		return nil, ScalarRange{}, err
	}

	candidates := encodeCandidates(source, span, style, newValue)
	if span.implicit {
		for i := range candidates {
			candidates[i] = append(append([]byte{' '}, candidates[i]...), trailingGap(span)...)
		}
	}

	var encoded []byte
	for _, cand := range candidates {
		probe := buildSource(source, span, cand)
		newFile, perr := parser.ParseBytes(probe, parser.ParseComments)
		if perr != nil {
			continue
		}
		if verifyReplacement(oldEntries, newFile, span, cand, newValue) {
			encoded = cand
			break
		}
	}
	if encoded == nil {
		return nil, ScalarRange{}, errors.New("could not encode replacement value without losing information")
	}

	result := buildSource(source, span, encoded)
	newFile, err := parser.ParseBytes(result, parser.ParseComments)
	if err != nil {
		return nil, ScalarRange{}, fmt.Errorf("replacement produced invalid YAML: %w", err)
	}

	newStart, err := replacementStart(oldEntries, newFile, span, encoded)
	if err != nil {
		return nil, ScalarRange{}, err
	}

	return result, ScalarRange{Start: newStart, End: newStart + len(encoded)}, nil
}

func trailingGap(span scalarSpan) []byte {
	if span.needsTrailingGap {
		return []byte{' '}
	}
	return nil
}

func isImplicitNullNode(node ast.Node) bool {
	if nn, ok := node.(*ast.NullNode); ok {
		return nn.Token != nil && nn.Token.Type == token.ImplicitNullType
	}
	return false
}

func nodeStart(node ast.Node, source []byte) (int, error) {
	if node == nil || node.GetToken() == nil || node.GetToken().Position == nil {
		return 0, ErrScalarNodeRequired
	}
	start := node.GetToken().Position.Offset - 1
	if start < 0 || start > len(source) {
		return 0, ErrScalarPositionMismatch
	}
	if _, ok := unwrapScalar(node); !ok {
		return 0, ErrScalarNodeRequired
	}
	return start, nil
}

func buildSource(source []byte, span scalarSpan, encoded []byte) []byte {
	result := make([]byte, 0, len(source)-span.len()+len(encoded))
	result = append(result, source[:span.start]...)
	result = append(result, encoded...)
	result = append(result, source[span.end:]...)
	return result
}

// scalarEntry is one decoded scalar occurrence (mapping keys included) in
// document order. Structurally identical documents produce equal slices.
type scalarEntry struct {
	start int
	value interface{}
	text  string
	block bool
}

type scalarCollector struct{ entries []scalarEntry }

// Visit records scalars in document order. A block scalar (LiteralNode) is
// recorded as a single scalar; its inner content StringNode is skipped.
func (c *scalarCollector) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}
	if lit, ok := n.(*ast.LiteralNode); ok {
		var value interface{} = ""
		if lit.Value != nil {
			value = lit.Value.Value
		}
		c.entries = append(c.entries, scalarEntry{
			start: lit.Start.Position.Offset - 1,
			value: value,
			text:  lit.Start.Value,
			block: true,
		})
		// Do not descend into the block's content node.
		return nil
	}
	value, ok := decodedScalarValue(n)
	if !ok {
		return c
	}
	tk := n.GetToken()
	if tk == nil || tk.Position == nil {
		return c
	}
	c.entries = append(c.entries, scalarEntry{
		start: tk.Position.Offset - 1,
		value: value,
		text:  tk.Value,
		block: false,
	})
	return c
}

func isBlockNode(n ast.Node) bool {
	_, ok := n.(*ast.LiteralNode)
	return ok
}

func collectScalars(f *ast.File) []scalarEntry {
	c := &scalarCollector{}
	for _, doc := range f.Docs {
		ast.Walk(c, doc)
	}
	return c.entries
}

func decodedScalarValue(n ast.Node) (interface{}, bool) {
	switch v := n.(type) {
	case *ast.StringNode:
		return v.Value, true
	case *ast.LiteralNode:
		if v.Value != nil {
			return v.Value.Value, true
		}
		return "", true
	case *ast.NullNode:
		return nil, true
	case *ast.BoolNode:
		return v.Value, true
	case *ast.IntegerNode:
		return v.Value, true
	case *ast.FloatNode:
		return v.Value, true
	case *ast.InfinityNode:
		return v.Value, true
	case *ast.NanNode:
		return math.NaN(), true
	default:
		return nil, false
	}
}

func unwrapScalar(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.StringNode, *ast.LiteralNode, *ast.NullNode, *ast.BoolNode,
		*ast.IntegerNode, *ast.FloatNode, *ast.InfinityNode, *ast.NanNode:
		return n, true
	}
	return nil, false
}

// locateOldScalar finds the scalar at start in a fresh parse of source and
// confirms it decodes to the same value as the caller-supplied node. This
// rejects stale nodes whose positions no longer describe source.
func locateOldScalar(f *ast.File, start int, want ast.Node) ([]scalarEntry, scalarStyle, error) {
	entries := collectScalars(f)
	wantValue, _ := decodedScalarValue(want)
	for _, e := range entries {
		if e.start != start || e.block != isBlockNode(want) {
			continue
		}
		if !scalarValuesEqual(e.value, wantValue) {
			return nil, 0, fmt.Errorf("%w: value at position changed", ErrScalarPositionMismatch)
		}
		style := styleOfNode(f, start)
		if style < 0 {
			return nil, 0, ErrScalarPositionMismatch
		}
		return entries, style, nil
	}
	return nil, 0, ErrScalarPositionMismatch
}

func styleOfNode(f *ast.File, start int) scalarStyle {
	found := stylePlain
	var matched bool
	for _, doc := range f.Docs {
		ast.Walk(styleFinder{start: start, out: &found, ok: &matched}, doc)
	}
	if !matched {
		return -1
	}
	return found
}

type styleFinder struct {
	start int
	out   *scalarStyle
	ok    *bool
}

func (v styleFinder) Visit(n ast.Node) ast.Visitor {
	if n == nil || !isScalarStyleNode(n) {
		return v
	}
	tk := n.GetToken()
	if tk == nil || tk.Position == nil {
		return v
	}
	if tk.Position.Offset-1 != v.start {
		return v
	}
	switch s := n.(type) {
	case *ast.StringNode:
		*v.ok = true
		switch s.Token.Type {
		case token.SingleQuoteType:
			*v.out = styleSingleQuote
		case token.DoubleQuoteType:
			*v.out = styleDoubleQuote
		default:
			*v.out = stylePlain
		}
	case *ast.LiteralNode:
		*v.ok = true
		if s.Start.Type == token.FoldedType || strings.HasPrefix(s.Start.Value, ">") {
			*v.out = styleFolded
		} else {
			*v.out = styleLiteral
		}
	default:
		if _, ok := decodedScalarValue(n); ok {
			*v.ok = true
			*v.out = stylePlain
		}
	}
	return v
}

func scalarValuesEqual(a, b interface{}) bool {
	if af, aok := a.(float64); aok {
		if bf, bok := b.(float64); bok && math.IsNaN(af) && math.IsNaN(bf) {
			return true
		}
	}
	return reflect.DeepEqual(a, b)
}

func isScalarStyleNode(n ast.Node) bool {
	switch n.(type) {
	case *ast.StringNode, *ast.LiteralNode, *ast.NullNode, *ast.BoolNode,
		*ast.IntegerNode, *ast.FloatNode, *ast.InfinityNode, *ast.NanNode:
		return true
	}
	return false
}

// verifyReplacement ensures the decoded scalar at the target slot equals
// newValue and every other scalar keeps the same decoded value.
func verifyReplacement(oldEntries []scalarEntry, newFile *ast.File, span scalarSpan, encoded []byte, newValue string) bool {
	newEntries := collectScalars(newFile)
	if len(oldEntries) != len(newEntries) {
		return false
	}
	for i := range oldEntries {
		o, n := oldEntries[i], newEntries[i]
		if o.start == span.targetStart && o.block == span.targetIsBlock() {
			nv, ok := n.value.(string)
			if !ok || nv != newValue {
				return false
			}
			continue
		}
		if !scalarValuesEqual(o.value, n.value) {
			return false
		}
	}
	return true
}

func (s scalarSpan) targetIsBlock() bool {
	return s.blockKind == '|' || s.blockKind == '>'
}

// replacementStart locates the target scalar in the new parse and returns
// its byte offset; every byte outside the replaced range is untouched by
// construction, so offsets before the span stay identical and offsets after
// it shift by exactly len(encoded) - span.len().
func replacementStart(oldEntries []scalarEntry, newFile *ast.File, span scalarSpan, encoded []byte) (int, error) {
	newEntries := collectScalars(newFile)
	for i := range oldEntries {
		if oldEntries[i].start == span.targetStart && oldEntries[i].block == span.targetIsBlock() {
			return newEntries[i].start, nil
		}
	}
	return 0, fmt.Errorf("%w: target scalar disappeared after replacement", ErrScalarPositionMismatch)
}
