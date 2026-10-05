package yaml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

func replace(t *testing.T, src, path, value, want string) {
	t.Helper()
	node := mustNode(t, src, path)
	out, rng, err := yaml.ReplaceScalar([]byte(src), node, value)
	if err != nil {
		t.Fatalf("ReplaceScalar(%s, %q) error: %v", path, value, err)
	}
	if got := string(out); got != want {
		t.Fatalf("ReplaceScalar(%s, %q)\n got: %q\nwant: %q", path, value, got, want)
	}
	if rng.Start < 0 || rng.End > len(out) || rng.Start > rng.End {
		t.Fatalf("invalid range %+v for output len %d", rng, len(out))
	}
	replaced := string(out[rng.Start:rng.End])
	if !strings.Contains(want, replaced) {
		t.Fatalf("range bytes %q not part of output", replaced)
	}
	assertDecodes(t, out, path, value)
}

func assertDecodes(t *testing.T, out []byte, path, wantValue string) {
	t.Helper()
	f, err := parser.ParseBytes(out, parser.ParseComments)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	p, err := yaml.PathString(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.FilterFile(f)
	if err != nil {
		t.Fatalf("re-locate %s: %v\nout=%q", path, err, string(out))
	}
	var got interface{}
	if l, ok := n.(*ast.LiteralNode); ok {
		if l.Value != nil {
			got = l.Value.Value
		} else {
			got = ""
		}
	} else {
		got = n.GetToken().Value
	}
	if s, ok := got.(string); !ok || s != wantValue {
		t.Fatalf("path %s decoded to %#v, want %q", path, got, wantValue)
	}
}

func mustNode(t *testing.T, src, path string) ast.Node {
	t.Helper()
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	p, err := yaml.PathString(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.FilterFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReplaceScalarPlain(t *testing.T) {
	const src = "a: hello\nb: keep # tail\n"
	replace(t, src, "$.a", "world", "a: world\nb: keep # tail\n")
	replace(t, src, "$.b", "other", "a: hello\nb: other # tail\n")
}

func TestReplaceScalarPlainNeedsQuoting(t *testing.T) {
	const src = "a: hello\nb: 1\n"
	replace(t, src, "$.a", "x # y", "a: 'x # y'\nb: 1\n")
	replace(t, src, "$.a", "x: y", "a: 'x: y'\nb: 1\n")
	replace(t, src, "$.a", " leading", "a: ' leading'\nb: 1\n")
	replace(t, src, "$.a", "trailing ", "a: 'trailing '\nb: 1\n")
	replace(t, src, "$.a", "", "a: ''\nb: 1\n")
	replace(t, src, "$.a", "line1\nline2\n", "a: \"line1\\nline2\\n\"\nb: 1\n")
	replace(t, src, "$.a", "true", "a: 'true'\nb: 1\n")
	replace(t, src, "$.a", "42", "a: '42'\nb: 1\n")
}

func TestReplaceScalarQuoted(t *testing.T) {
	replace(t, "a: 'world'\nb: 1\n", "$.a", "it's", "a: 'it''s'\nb: 1\n")
	replace(t, "a: \"x\\ny\"\nb: 1\n", "$.a", "a\nb", "a: \"a\\nb\"\nb: 1\n")
	replace(t, "a: 'world'\nb: 1\n", "$.a", "two\nlines\n", "a: \"two\\nlines\\n\"\nb: 1\n")
	replace(t, "a: 'x' # c\nb: 1\n", "$.a", "y", "a: 'y' # c\nb: 1\n")
}

func TestReplaceScalarLiteral(t *testing.T) {
	replace(t, "a: |\n  line1\n  line2\nb: 1\n", "$.a", "new1\nnew2\n",
		"a: |\n  new1\n  new2\nb: 1\n")
	replace(t, "a: |-\n  line1\nb: 1\n", "$.a", "no-newline",
		"a: |-\n  no-newline\nb: 1\n")
	replace(t, "a: |+\n  line1\n\n\nb: 1\n", "$.a", "x\n\n",
		"a: |+\n  x\n\nb: 1\n")
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "",
		"a: |\nb: 1\n")
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "  indented\n",
		"a: |2\n    indented\nb: 1\n")
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "a\n    b\n",
		"a: |\n  a\n      b\nb: 1\n")
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "\n\n",
		"a: |+\n  \n\nb: 1\n")
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "\nx\n",
		"a: \"\\nx\\n\"\nb: 1\n")
}

func TestReplaceScalarLiteralAtEOF(t *testing.T) {
	replace(t, "a: |-\n  old", "$.a", "new", "a: |-\n  new")
	replace(t, "a: |\n  old\n", "$.a", "new\n", "a: |\n  new\n")
}

func TestReplaceScalarFoldedDoesNotFold(t *testing.T) {
	// A value containing a hard newline must not be folded into a space.
	replace(t, "a: >\n  old\n  text\nb: 1\n", "$.a", "keep\nbreak\n",
		"a: |\n  keep\n  break\nb: 1\n")
	replace(t, "a: >\n  old\n  text\nb: 1\n", "$.a", "a\n\nb\n",
		"a: |\n  a\n\n  b\nb: 1\n")
	// A single-line value stays folded.
	replace(t, "a: >\n  old\nb: 1\n", "$.a", "single", "a: >-\n  single\nb: 1\n")
}

func TestReplaceScalarPreservesBytes(t *testing.T) {
	src := "a: hello\r\nb: keep # tail\r\n"
	replace(t, src, "$.a", "world", "a: world\r\nb: keep # tail\r\n")

	// CRLF inside a block scalar.
	replace(t, "a: |\r\n  old\r\nb: 1\r\n", "$.a", "new\ndata\n",
		"a: |\r\n  new\r\n  data\r\nb: 1\r\n")

	// Document markers untouched.
	replace(t, "---\na: x\n...\n", "$.a", "y", "---\na: y\n...\n")

	// Same-line comment on a block header is retained.
	replace(t, "a: | # head\n  old\nb: 1\n", "$.a", "new\n",
		"a: | # head\n  new\nb: 1\n")

	// Nested block keeps everything outside the span identical.
	replace(t, "a:\n  b: |\n    x\n  c: 1\n", "$.a.b", "yy\nzz\n",
		"a:\n  b: |\n    yy\n    zz\n  c: 1\n")
}

func TestReplaceScalarImplicitNull(t *testing.T) {
	replace(t, "a:\nb: 1\n", "$.a", "v", "a: v\nb: 1\n")
	replace(t, "a: # note\nb: 1\n", "$.a", "v", "a: v # note\nb: 1\n")
	replace(t, "a:\nb: 1\n", "$.a", "x\ny\n", "a: \"x\\ny\\n\"\nb: 1\n")
	replace(t, "- \n- 2\n", "$[0]", "v", "- v\n- 2\n")
}

func TestReplaceScalarSequence(t *testing.T) {
	replace(t, "- a\n- b\n", "$[1]", "z", "- a\n- z\n")
}

func TestReplaceScalarMappingKey(t *testing.T) {
	src := "oldkey: 1\nx: 2\n"
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var key ast.Node
	ast.Walk(keyFinder{name: "oldkey", out: &key}, f.Docs[0])
	if key == nil {
		t.Fatal("key not found")
	}
	out, rng, err := yaml.ReplaceScalar([]byte(src), key, "newkey")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "newkey: 1\nx: 2\n" {
		t.Fatalf("got %q", string(out))
	}
	if want := "newkey"; string(out[rng.Start:rng.End]) != want {
		t.Fatalf("range %q", string(out[rng.Start:rng.End]))
	}
}

type keyFinder struct {
	name string
	out  *ast.Node
}

func (f keyFinder) Visit(n ast.Node) ast.Visitor {
	if mv, ok := n.(*ast.MappingValueNode); ok {
		if s, ok := mv.Key.(*ast.StringNode); ok && s.Value == f.name {
			*f.out = s
			return nil
		}
	}
	return f
}

func TestReplaceScalarStaleNodeRejected(t *testing.T) {
	f, err := parser.ParseBytes([]byte("a: hello\nb: 1\n"), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := yaml.PathString("$.a")
	n, err := p.FilterFile(f)
	if err != nil {
		t.Fatal(err)
	}
	// Source changed after parsing; recorded offsets are stale.
	if _, _, err := yaml.ReplaceScalar([]byte("zz: hello\nb: 1\n"), n, "v"); !errors.Is(err, yaml.ErrScalarPositionMismatch) {
		t.Fatalf("expected ErrScalarPositionMismatch, got %v", err)
	}
}

func TestReplaceScalarNonScalarRejected(t *testing.T) {
	f, err := parser.ParseBytes([]byte("a:\n  b: 1\n"), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := yaml.PathString("$.a")
	n, err := p.FilterFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := yaml.ReplaceScalar([]byte("a:\n  b: 1\n"), n, "v"); !errors.Is(err, yaml.ErrScalarNodeRequired) {
		t.Fatalf("expected ErrScalarNodeRequired, got %v", err)
	}
}

func TestReplaceScalarUnchangedOthers(t *testing.T) {
	src := "k: 'keep'\nv: 1\nb: true\nn: null\na: |\n  block\nz: end\n"
	out, _, err := yaml.ReplaceScalar([]byte(src), mustNode(t, src, "$.v"), "42")
	if err != nil {
		t.Fatal(err)
	}
	want := "k: 'keep'\nv: '42'\nb: true\nn: null\na: |\n  block\nz: end\n"
	if string(out) != want {
		t.Fatalf("got %q want %q", string(out), want)
	}
}

func TestReplaceScalarMultiDocument(t *testing.T) {
	src := "---\na: x\n---\na: y\n"
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var target ast.Node
	ast.Walk(secondValueFinder{out: &target}, f.Docs[1])
	out, _, err := yaml.ReplaceScalar([]byte(src), target, "Z")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "---\na: x\n---\na: Z\n" {
		t.Fatalf("got %q", string(out))
	}
}

type secondValueFinder struct{ out *ast.Node }

func (f secondValueFinder) Visit(n ast.Node) ast.Visitor {
	if s, ok := n.(*ast.StringNode); ok && s.Value == "y" {
		*f.out = s
		return nil
	}
	return f
}

func TestReplaceScalarFlow(t *testing.T) {
	replace(t, "a: [one, two]\n", "$.a[1]", "three", "a: [one, three]\n")
	replace(t, "a: {x: 1, y: 2}\n", "$.a.y", "nine", "a: {x: 1, y: nine}\n")
	replace(t, "a: {x: 1, y: 2}\n", "$.a.y", "9", "a: {x: 1, y: '9'}\n")
}

func TestReplaceScalarDocumentWithoutMarker(t *testing.T) {
	replace(t, "a: x\nb: 1\n", "$.a", "yy", "a: yy\nb: 1\n")
}

func TestReplaceScalarSpecialBytes(t *testing.T) {
	// A value that cannot stay plain must be quoted; the decoded value is
	// verified byte-for-byte.
	for _, in := range []string{"#leading", "word:", "a\tb", "a\u0007b", "hé→世"} {
		src := "a: x\nb: 1\n"
		node := mustNode(t, src, "$.a")
		out, _, err := yaml.ReplaceScalar([]byte(src), node, in)
		if err != nil {
			t.Fatalf("value %q: %v", in, err)
		}
		assertDecodes(t, out, "$.a", in)
	}
}

func TestReplaceScalarRangeExact(t *testing.T) {
	src := "a: hello\nb: 1\n"
	node := mustNode(t, src, "$.a")
	out, rng, err := yaml.ReplaceScalar([]byte(src), node, "world")
	if err != nil {
		t.Fatal(err)
	}
	// Bytes before the range and after it are identical to the source.
	if string(out[:rng.Start]) != src[:3] || string(out[rng.End:]) != src[8:] {
		t.Fatalf("outside bytes changed: %q", string(out))
	}
}

func TestReplaceScalarChompingTransitions(t *testing.T) {
	// clip -> strip (no trailing newline)
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "bare", "a: |-\n  bare\nb: 1\n")
	// strip -> clip
	replace(t, "a: |-\n  old\nb: 1\n", "$.a", "with\n", "a: |\n  with\nb: 1\n")
	// clip -> keep with several trailing newlines
	replace(t, "a: |\n  old\nb: 1\n", "$.a", "x\n\n\n",
		"a: |+\n  x\n\n\nb: 1\n")
	// keep -> clip
	replace(t, "a: |+\n  old\n\n\nb: 1\n", "$.a", "x\n",
		"a: |\n  x\nb: 1\n")
}

func TestReplaceScalarFoldedEmpty(t *testing.T) {
	replace(t, "a: >\n  old\nb: 1\n", "$.a", "", "a: >\nb: 1\n")
	replace(t, "a: >\n  old\nb: 1\n", "$.a", "one", "a: >-\n  one\nb: 1\n")
}

func TestReplaceScalarReturnsNewRange(t *testing.T) {
	src := "a: hi\nb: 1\n"
	node := mustNode(t, src, "$.a")
	out, rng, err := yaml.ReplaceScalar([]byte(src), node, "much longer value: yes")
	if err != nil {
		t.Fatal(err)
	}
	if string(out[rng.Start:rng.End]) != "'much longer value: yes'" {
		t.Fatalf("range %q", string(out[rng.Start:rng.End]))
	}
	if rng.Start != 3 {
		t.Fatalf("start = %d", rng.Start)
	}
}

func TestReplaceScalarCRLFPlain(t *testing.T) {
	src := "a: hello\r\nb: 1\r\n"
	node := mustNode(t, src, "$.a")
	out, rng, err := yaml.ReplaceScalar([]byte(src), node, "x: y")
	if err != nil {
		t.Fatal(err)
	}
	want := "a: 'x: y'\r\nb: 1\r\n"
	if string(out) != want {
		t.Fatalf("got %q want %q", string(out), want)
	}
	if string(out[rng.Start:rng.End]) != "'x: y'" {
		t.Fatalf("range %q", string(out[rng.Start:rng.End]))
	}
}
