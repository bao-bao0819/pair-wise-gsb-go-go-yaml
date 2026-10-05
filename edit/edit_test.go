package edit_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/edit"
	"github.com/goccy/go-yaml/parser"
)

// nodeAt finds the scalar leaf at a 1-based line/column in src.
func nodeAt(t *testing.T, src string, line, col int) ast.Node {
	t.Helper()
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found ast.Node
	for _, doc := range f.Docs {
		walkForNode(doc, line, col, &found)
		if found != nil {
			break
		}
	}
	if found == nil {
		t.Fatalf("no scalar node at %d:%d", line, col)
	}
	return found
}

func walkForNode(n ast.Node, line, col int, found *ast.Node) {
	if n == nil || *found != nil {
		return
	}
	if n.GetToken() != nil && n.GetToken().Position != nil &&
		n.GetToken().Position.Line == line && n.GetToken().Position.Column == col {
		if _, ok := n.(*ast.MappingValueNode); !ok {
			*found = n
			return
		}
	}
	switch v := n.(type) {
	case *ast.DocumentNode:
		walkForNode(v.Body, line, col, found)
	case *ast.MappingNode:
		for _, mv := range v.Values {
			walkForNode(mv, line, col, found)
		}
	case *ast.MappingValueNode:
		walkForNode(v.Key, line, col, found)
		walkForNode(v.Value, line, col, found)
	case *ast.SequenceNode:
		for _, e := range v.Values {
			walkForNode(e, line, col, found)
		}
	case *ast.SequenceEntryNode:
		walkForNode(v.Value, line, col, found)
	case *ast.LiteralNode:
		*found = v
	}
}

func assertReplace(t *testing.T, src string, line, col int, value, want string) {
	t.Helper()
	node := nodeAt(t, src, line, col)
	out, rng, err := edit.ReplaceScalar([]byte(src), node, value)
	if err != nil {
		t.Fatalf("ReplaceScalar(%q -> %q): %v", src, value, err)
	}
	if string(out) != want {
		t.Fatalf("src=%q value=%q\ngot:  %q\nwant: %q", src, value, out, want)
	}
	if rng.StartOffset < 0 || rng.EndOffset < rng.StartOffset ||
		rng.StartOffset > len(src) || rng.EndOffset > len(src) {
		t.Fatalf("invalid range %v for src len %d", rng, len(src))
	}
}

func assertReject(t *testing.T, src string, node ast.Node) {
	t.Helper()
	_, _, err := edit.ReplaceScalar([]byte(src), node, "x")
	if err == nil {
		t.Fatalf("expected rejection for %q", src)
	}
}

func TestPlainScalar(t *testing.T) {
	assertReplace(t, "a: hello world\nb: 2\n", 1, 4, "new val",
		"a: new val\nb: 2\n")
}

func TestEmptyStringEscalatesStyle(t *testing.T) {
	// Plain style cannot represent an empty string; switch to quotes.
	assertReplace(t, "a: hello\nb: 2\n", 1, 4, "",
		"a: ''\nb: 2\n")
	assertReplace(t, "a: |\n  x\nb: 2\n", 1, 4, "",
		"a: ''\nb: 2\n")
}

func TestHashAndColonContent(t *testing.T) {
	assertReplace(t, "a: hello\nb: 2\n", 1, 4, "v#x: y",
		"a: 'v#x: y'\nb: 2\n")
	assertReplace(t, "a: hello\nb: 2\n", 1, 4, "key: value",
		"a: 'key: value'\nb: 2\n")
	// '#' always escalates to quoting with this library's conservative
	// check; the value itself is preserved losslessly.
	assertReplace(t, "a: hello\nb: 2\n", 1, 4, "foo#bar",
		"a: 'foo#bar'\nb: 2\n")
}

func TestLeadingSpace(t *testing.T) {
	assertReplace(t, "a: hello\nb: 2\n", 1, 4, " leading",
		"a: ' leading'\nb: 2\n")
}

func TestQuotedScalars(t *testing.T) {
	// Single quotes stay single, apostrophe doubled.
	assertReplace(t, "b: 'it''s'\nc: 2\n", 1, 4, "x'y",
		"b: 'x''y'\nc: 2\n")
	// Double quotes stay double, tab/newline escaped.
	assertReplace(t, "c: \"x\\ty\"\nd: 2\n", 1, 4, "a\tb\nc",
		"c: \"a\\tb\\nc\"\nd: 2\n")
	// Single quoted new value with newline falls back to double.
	assertReplace(t, "b: 'abc'\nc: 2\n", 1, 4, "a\nb",
		"b: \"a\\nb\"\nc: 2\n")
}

func TestLiteralScalarChomping(t *testing.T) {
	// clip with single trailing newline stays "|".
	assertReplace(t, "d: |\n  line1\n  line2\ne: x\n", 1, 4, "new\nbody\n",
		"d: |\n  new\n  body\ne: x\n")
	// No trailing newline -> strip.
	assertReplace(t, "d: |\n  line1\ne: x\n", 1, 4, "nonewline",
		"d: |-\n  nonewline\ne: x\n")
	// Multiple trailing newlines -> keep.
	assertReplace(t, "f: |\n  keep\ng: end\n", 1, 4, "keep\n\n\n",
		"f: |+\n  keep\n\n\ng: end\n")
	// Existing keep block reduced to strip drops blank lines.
	assertReplace(t, "f: |+\n  keep\n\n\ng: end\n", 1, 4, "x",
		"f: |-\n  x\ng: end\n")
}

func TestLiteralScalarAtEOF(t *testing.T) {
	assertReplace(t, "a: |\n  x\n", 1, 4, "z",
		"a: |-\n  z")
	assertReplace(t, "a: |\n  x\n", 1, 4, "z\n",
		"a: |\n  z\n")
	assertReplace(t, "a: |\n  x\n", 1, 4, "z\n\n",
		"a: |+\n  z\n\n")
}

func TestFoldedScalarNeverFoldsNewlines(t *testing.T) {
	// Folded value without newlines can stay folded.
	assertReplace(t, "e: >-\n  fold\n  next\ng: end\n", 1, 4, "word",
		"e: >-\n  word\ng: end\n")
	// Folded value with newlines switches to literal so newlines survive.
	assertReplace(t, "e: >-\n  fold\n  next\ng: end\n", 1, 4, "a\nb",
		"e: |-\n  a\n  b\ng: end\n")
}

func TestBlockExplicitIndent(t *testing.T) {
	assertReplace(t, "a: |2\n    lead\nb: 2\n", 1, 4, "  leading",
		"a: |2-\n    leading\nb: 2\n")
	// Previously auto indented block whose new value starts with a space
	// gains an explicit indicator.
	assertReplace(t, "a: |\n  x\nb: 2\n", 1, 4, " one",
		"a: |2-\n   one\nb: 2\n")
}

func TestBlockInSequence(t *testing.T) {
	assertReplace(t, "items:\n  - |\n    aa\n    bb\n  - x\n", 2, 5, "cc\ndd",
		"items:\n  - |-\n    cc\n    dd\n  - x\n")
	// Empty block followed immediately by another sequence entry.
	assertReplace(t, "items:\n  - |\n  - x\n", 2, 5, "cc\ndd",
		"items:\n  - |-\n    cc\n    dd\n  - x\n")
}

func TestCRLFPreserved(t *testing.T) {
	assertReplace(t, "a: x\r\nb: 2\r\n", 1, 4, "z",
		"a: z\r\nb: 2\r\n")
	assertReplace(t, "a: |\r\n  l1\r\n  l2\r\nb: 2\r\n", 1, 4, "n1\nn2",
		"a: |-\r\n  n1\r\n  n2\r\nb: 2\r\n")
}

func TestCommentsAndDocSeparatorsPreserved(t *testing.T) {
	// Same line comment next to a plain scalar is untouched.
	assertReplace(t, "a: hello # c\nb: 2\n", 1, 4, "zz",
		"a: zz # c\nb: 2\n")
	// Header comment of a block scalar is carried over.
	assertReplace(t, "a: | # hello\n  x\nb: 2\n", 1, 4, "y\nz",
		"a: |- # hello\n  y\n  z\nb: 2\n")
	// Document separator after a block is preserved.
	assertReplace(t, "a: |\n  x\n---\nb: 2\n", 1, 4, "yy",
		"a: |-\n  yy\n---\nb: 2\n")
}

func TestTypedScalars(t *testing.T) {
	assertReplace(t, "h: 123\nk: x\n", 1, 4, "456",
		"h: 456\nk: x\n")
	assertReplace(t, "h: 123\nk: x\n", 1, 4, "abc",
		"h: 'abc'\nk: x\n")
	assertReplace(t, "h: abc\nk: x\n", 1, 4, "456",
		"h: '456'\nk: x\n")
	assertReplace(t, "h: true\nk: x\n", 1, 4, "false",
		"h: false\nk: x\n")
	assertReplace(t, "h: 1.5\nk: x\n", 1, 4, "2.5",
		"h: 2.5\nk: x\n")
}

func TestFlowCollections(t *testing.T) {
	assertReplace(t, "a: [one, two]\nb: 2\n", 1, 5, "zz",
		"a: [zz, two]\nb: 2\n")
	assertReplace(t, "a: {k: one}\nb: 2\n", 1, 8, "zz",
		"a: {k: zz}\nb: 2\n")
}

func TestMultiLinePlain(t *testing.T) {
	assertReplace(t, "a: hello\n  world\nb: 2\n", 1, 4, "z",
		"a: z\nb: 2\n")
}

func TestRangeReportsReplacedBytes(t *testing.T) {
	src := "a: hello\nb: 2\n"
	node := nodeAt(t, src, 1, 4)
	out, rng, err := edit.ReplaceScalar([]byte(src), node, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if got := src[rng.StartOffset:rng.EndOffset]; got != "hello" {
		t.Fatalf("range covers %q, want hello", got)
	}
	if rng.StartLine != 1 || rng.StartColumn != 4 || rng.EndLine != 1 || rng.EndColumn != 8 {
		t.Fatalf("unexpected coordinates: %+v", rng)
	}
	if string(out[:rng.StartOffset]) != src[:rng.StartOffset] {
		t.Fatal("prefix changed")
	}
	if string(out[rng.StartOffset+len("hi"):]) != src[rng.EndOffset:] {
		t.Fatal("suffix changed")
	}
}

func TestStalePositionRejected(t *testing.T) {
	srcA := "a: hello\nb: 2\n"
	node := nodeAt(t, srcA, 1, 4)

	// Node parsed from one source applied to a different source.
	assertReject(t, "a: different\nother: 9\n", node)
	// Content changed at the same coordinates.
	assertReject(t, "a: world\nb: 2\n", node)
	// Shifted columns.
	assertReject(t, "xx: hello\nb: 2\n", node)

	// A node from a quoted source must not match a plain scalar.
	quotedNode := nodeAt(t, "a: 'v'\nb: 2\n", 1, 4)
	assertReject(t, "a: v\nb: 2\n", quotedNode)

	// A block scalar whose content changed but header coordinates match.
	blockNode := nodeAt(t, "a: |\n  x\nb: 2\n", 1, 4)
	assertReject(t, "a: |\n  DIFFERENT\nb: 2\n", blockNode)
	assertReject(t, "a: |\n  x\n  y\nb: 2\n", blockNode)

	// nil node.
	if _, _, err := edit.ReplaceScalar([]byte(srcA), nil, "x"); !errors.Is(err, edit.ErrUnsupportedNode) {
		t.Fatalf("got %v, want ErrUnsupportedNode", err)
	}
}

func TestOtherBytesUnchanged(t *testing.T) {
	// Replace a value and ensure every byte outside the range is equal,
	// including comments, indentation and separators.
	src := "# header\na: hello # tail\nb:\n  - v1\n  - v2 # note\nc: |+\n  x\n\n\n---\nd: end\n"
	node := nodeAt(t, src, 2, 4)
	out, rng, err := edit.ReplaceScalar([]byte(src), node, "zz")
	if err != nil {
		t.Fatal(err)
	}
	replaced := []byte("zz")
	want := make([]byte, 0, len(src))
	want = append(want, src[:rng.StartOffset]...)
	want = append(want, replaced...)
	want = append(want, src[rng.EndOffset:]...)
	if string(out) != string(want) {
		t.Fatalf("byte exactness violated\ngot:  %q\nwant: %q", out, want)
	}
	if !strings.Contains(string(out), "# header") ||
		!strings.Contains(string(out), "# tail") ||
		!strings.Contains(string(out), "# note") ||
		!strings.Contains(string(out), "---") {
		t.Fatal("comments or separators lost")
	}
}
