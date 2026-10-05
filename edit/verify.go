package edit

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// scalarEntry is one scalar leaf in a document, identified by its path
// and ordered by source position.
type scalarEntry struct {
	path  string
	value string
	line  int
	col   int
	style scalarStyle
	kind  string
}

// collectScalars parses src and returns all scalar leaves in source order.
func collectScalars(src string) ([]scalarEntry, error) {
	file, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []scalarEntry
	walkScalars(file, func(n ast.Node) {
		style, ok := nodeStyle(n)
		if !ok {
			return
		}
		tk := n.GetToken()
		out = append(out, scalarEntry{
			path:  n.GetPath(),
			value: scalarCanonical(n),
			line:  tk.Position.Line,
			col:   tk.Position.Column,
			style: style,
			kind:  tk.Type.String(),
		})
	})
	return out, nil
}

// scalarCanonical returns the decoded value of a scalar leaf as a string.
func scalarCanonical(n ast.Node) string {
	if lit, ok := n.(*ast.LiteralNode); ok {
		if lit.Value != nil {
			return lit.Value.Value
		}
		return ""
	}
	if s, ok := n.(*ast.StringNode); ok {
		return s.Value
	}
	// Typed scalars (null/bool/int/float): use the token value.
	return n.GetToken().Value
}

// verifyReplacement parses both src and out, checks the target scalar now
// equals newValue and every other scalar keeps its exact value.
func verifyReplacement(src, out, targetPath string, targetLine, targetCol int, targetKind string, newValue string) error {
	before, err := collectScalars(src)
	if err != nil {
		return fmt.Errorf("yaml/edit: cannot parse original source: %w", err)
	}
	after, err := collectScalars(out)
	if err != nil {
		return fmt.Errorf("yaml/edit: replacement produced invalid YAML: %w", err)
	}
	if len(before) != len(after) {
		return fmt.Errorf("yaml/edit: structure changed (scalar count %d -> %d)", len(before), len(after))
	}

	targetHit := false
	for i := range before {
		b, a := before[i], after[i]
		if !targetHit && b.line == targetLine && b.col == targetCol && b.kind == targetKind {
			if a.value != newValue {
				return fmt.Errorf("yaml/edit: target value mismatch after edit: got %q want %q", a.value, newValue)
			}
			targetHit = true
			continue
		}
		// For non target scalars the value must remain equal. Paths may
		// shift when a mapping key itself is edited, so compare values by
		// ordered position and only treat a value difference as a change.
		if b.value != a.value || b.kind != a.kind {
			return fmt.Errorf("yaml/edit: unrelated scalar changed at %q: %q -> %q", b.path, b.value, a.value)
		}
	}
	if !targetHit {
		return fmt.Errorf("yaml/edit: target scalar %q missing after edit", targetPath)
	}
	return nil
}
