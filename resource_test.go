package yaml_test

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// exponentialBomb builds a YAML document of only a few dozen syntax nodes
// whose alias expansion grows exponentially:
//
//	a0: [0, 0]
//	a1: [*a0, *a0]
//	a2: [*a1, *a1]
//	...
//	root: *aN
//
// aN expands into 2^(N+1) logical nodes while the source stays linear.
func exponentialBomb(stages int) string {
	var b strings.Builder
	b.WriteString("a0: &a0 [0, 0]\n")
	for i := 1; i < stages; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [*a%d, *a%d]\n", i, i, i-1, i-1)
	}
	fmt.Fprintf(&b, "root: *a%d\n", stages-1)
	return b.String()
}

type nodeCounter struct{ count uint64 }

func (v *nodeCounter) Visit(n ast.Node) ast.Visitor {
	if n != nil {
		v.count++
	}
	return v
}

func countNodes(root ast.Node) uint64 {
	v := &nodeCounter{}
	ast.Walk(v, root)
	return v.count
}

// fileNodes returns the document nodes of f, mirroring the variadic
// [yaml.CheckResourceLimit] API for multi-document validation.
func fileNodes(f *ast.File) []ast.Node {
	nodes := make([]ast.Node, 0, len(f.Docs))
	for _, doc := range f.Docs {
		if doc != nil {
			nodes = append(nodes, doc)
		}
	}
	return nodes
}

func TestResourceLimit_RejectsHighAmplificationQuickly(t *testing.T) {
	const stages = 40
	src := exponentialBomb(stages)

	// Sanity: the input really is tiny (only a few dozen syntax nodes).
	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if got := countNodes(f.Docs[0]); got > 1000 {
		t.Fatalf("expected a few dozen syntax nodes, got %d", got)
	}

	limit := yaml.ResourceLimit{
		MaxSyntaxNodes:  10000,
		MaxAliasDepth:   100,
		MaxLogicalNodes: 10000,
	}

	// AST-only check mode rejects without decoding or expanding.
	err = yaml.CheckResourceLimit(limit, fileNodes(f)...)
	var rle *yaml.ResourceLimitError
	if err == nil || !errors.As(err, &rle) {
		t.Fatalf("expected *ResourceLimitError for amplified graph, got %v", err)
	}
	if rle.Kind != "logical nodes" {
		t.Fatalf("expected logical nodes violation, got %q: %v", rle.Kind, err)
	}
	if !yaml.IsResourceLimitError(err) {
		t.Fatalf("IsResourceLimitError = false for %v", err)
	}
	if len(rle.AliasPath) == 0 {
		t.Fatalf("expected non-empty alias path on overflow (%v)", err)
	}
	for _, name := range rle.AliasPath {
		if name == "" {
			t.Fatalf("empty entry in overflow path %#v", rle.AliasPath)
		}
	}
	t.Logf("bomb rejected via %s (count >= %d)", strings.Join(rle.AliasPath, " -> "), rle.Count)

	// Decoding with the limit fails fast and produces no giant object.
	var v any
	start := time.Now()
	err = yaml.UnmarshalWithOptions([]byte(src), &v, yaml.WithResourceLimit(limit))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected decode to be rejected")
	}
	if !yaml.IsResourceLimitError(err) {
		t.Fatalf("expected resource limit error, got %T: %v", err, err)
	}
	if v != nil {
		t.Fatalf("expected no value to be produced, got %#v", v)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("rejection took too long: %v", elapsed)
	}

	// The streaming decoder rejects before allocating the expansion as well.
	v = nil
	dec := yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimit(limit))
	if err := dec.Decode(&v); !yaml.IsResourceLimitError(err) {
		t.Fatalf("expected resource limit error from decoder, got %v", err)
	}
	if v != nil {
		t.Fatalf("expected no value from decoder, got %#v", v)
	}

	// A much larger graph must be rejected just as fast; the estimator never
	// materializes the astronomically large expansion.
	bigSrc := exponentialBomb(200)
	start = time.Now()
	if err := yaml.UnmarshalWithOptions([]byte(bigSrc), &v,
		yaml.WithResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: 10000}),
	); !yaml.IsResourceLimitError(err) {
		t.Fatalf("expected 200-stage bomb to be rejected, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("200-stage rejection took too long: %v", elapsed)
	}

	// The rejected expansion would require many exabytes if materialized;
	// ensure the actual heap delta stays small.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 10; i++ {
		var z any
		if err := yaml.UnmarshalWithOptions([]byte(src), &z,
			yaml.WithResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: 1000}),
		); !yaml.IsResourceLimitError(err) {
			t.Fatalf("iteration %d: expected rejection, got %v", i, err)
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if delta := int64(after.HeapAlloc) - int64(before.HeapAlloc); delta > 10<<20 {
		t.Fatalf("rejection allocated too much heap: %d bytes", delta)
	}
}

// repeatedSmallAnchor reuses one tiny anchor many times. It expands to a
// bounded, linear number of logical nodes and must decode fine within a
// sufficient budget.
func repeatedSmallAnchor(n int) string {
	var b strings.Builder
	b.WriteString("base: &base [1, 2, 3]\n")
	b.WriteString("items:\n")
	for i := 0; i < n; i++ {
		b.WriteString("  - *base\n")
	}
	return b.String()
}

func TestResourceLimit_AllowsLegalRepeatedAnchorWithinBudget(t *testing.T) {
	const repeats = 200
	src := repeatedSmallAnchor(repeats)

	var v map[string]any
	err := yaml.UnmarshalWithOptions([]byte(src), &v, yaml.WithResourceLimit(yaml.ResourceLimit{
		MaxSyntaxNodes:  100000,
		MaxAliasDepth:   10,
		MaxLogicalNodes: 1000000,
	}))
	if err != nil {
		t.Fatalf("expected successful decode, got %v", err)
	}
	items, ok := v["items"].([]any)
	if !ok {
		t.Fatalf("expected []any items, got %T", v["items"])
	}
	if len(items) != repeats {
		t.Fatalf("expected %d items, got %d", repeats, len(items))
	}
	first, ok := items[0].([]any)
	if !ok || len(first) != 3 {
		t.Fatalf("expected aliased [1 2 3], got %#v", items[0])
	}

	// A tight logical budget reports the alias path that tipped the budget.
	f, perr := parser.ParseBytes([]byte(src), 0)
	if perr != nil {
		t.Fatal(perr)
	}
	err = yaml.CheckResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: 10}, fileNodes(f)...)
	var rle *yaml.ResourceLimitError
	if err == nil || !errors.As(err, &rle) {
		t.Fatalf("expected logical nodes violation, got %v", err)
	}
	if len(rle.AliasPath) != 1 || rle.AliasPath[0] != "base" {
		t.Fatalf("expected overflow path [*base], got %#v (%v)", rle.AliasPath, err)
	}
}

func TestResourceLimit_AliasDepthReportsReferencePath(t *testing.T) {
	src := strings.Join([]string{
		"a: &a 1",
		"b: &b [*a]",
		"c: &c [*b]",
		"root: *c",
	}, "\n")

	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	err = yaml.CheckResourceLimit(yaml.ResourceLimit{MaxAliasDepth: 2}, fileNodes(f)...)
	var rle *yaml.ResourceLimitError
	if err == nil || !errors.As(err, &rle) {
		t.Fatalf("expected alias depth violation, got %v", err)
	}
	if rle.Kind != "alias depth" {
		t.Fatalf("expected alias depth kind, got %q", rle.Kind)
	}
	want := []string{"c", "b", "a"}
	if len(rle.AliasPath) != len(want) {
		t.Fatalf("expected path %v, got %v (%v)", want, rle.AliasPath, err)
	}
	for i := range want {
		if rle.AliasPath[i] != want[i] {
			t.Fatalf("expected path %v, got %v (%v)", want, rle.AliasPath, err)
		}
	}

	// Exactly enough depth decodes successfully.
	var v any
	if err := yaml.UnmarshalWithOptions([]byte(src), &v,
		yaml.WithResourceLimit(yaml.ResourceLimit{MaxAliasDepth: 3, MaxLogicalNodes: 1000}),
	); err != nil {
		t.Fatalf("expected decode within depth budget, got %v", err)
	}
}

func TestResourceLimit_SyntaxNodeLimitTraversesASTOnly(t *testing.T) {
	src := strings.Join([]string{
		"base: &base",
		"  x: 1",
		"  y: 2",
		"a: *base",
		"b: *base",
		"c: *base",
	}, "\n")

	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	total := countNodes(f.Docs[0])

	err = yaml.CheckResourceLimit(yaml.ResourceLimit{MaxSyntaxNodes: total - 1}, fileNodes(f)...)
	var rle *yaml.ResourceLimitError
	if err == nil || !errors.As(err, &rle) {
		t.Fatalf("expected syntax nodes violation, got %v", err)
	}
	if rle.Kind != "syntax nodes" {
		t.Fatalf("expected syntax nodes kind, got %q", rle.Kind)
	}
	if rle.AliasPath != nil {
		t.Fatalf("syntax violation must not carry an alias path, got %v", rle.AliasPath)
	}

	if err := yaml.CheckResourceLimit(yaml.ResourceLimit{MaxSyntaxNodes: total}, fileNodes(f)...); err != nil {
		t.Fatalf("expected pass at exact syntax budget, got %v", err)
	}

	// Alias sharing does not inflate the syntax count: the syntax walker
	// never follows alias edges.
	if err := yaml.CheckResourceLimit(yaml.ResourceLimit{
		MaxSyntaxNodes:  total,
		MaxLogicalNodes: 100,
	}, fileNodes(f)...); err != nil {
		t.Fatalf("logical budget covers the small expansion, got %v", err)
	}
}

func TestResourceLimit_AliasCycleReportedSeparately(t *testing.T) {
	for _, src := range []string{
		"a: &a\n  - *a\n",
		"a: &a\n  x: *a\n",
		"a: &a\n  b: &b\n    - *a\n    - *b\n",
	} {
		f, err := parser.ParseBytes([]byte(src), 0)
		if err != nil {
			t.Fatalf("parse error for %q: %v", src, err)
		}
		// A tiny depth budget must not turn the cycle into a plain depth
		// violation: the cycle wins.
		err = yaml.CheckResourceLimit(yaml.ResourceLimit{MaxAliasDepth: 1, MaxLogicalNodes: 5}, fileNodes(f)...)
		var ace *yaml.AliasCycleError
		if err == nil || !errors.As(err, &ace) {
			t.Fatalf("expected *AliasCycleError for %q, got %v", src, err)
		}
		if !yaml.IsAliasCycleError(err) {
			t.Fatalf("IsAliasCycleError = false for %v", err)
		}
		if len(ace.Path) == 0 {
			t.Fatalf("expected non-empty cycle path: %v", err)
		}
		if yaml.IsResourceLimitError(err) {
			t.Fatalf("cycle error must not be classified as a limit error: %v", err)
		}

		var v any
		if err := yaml.UnmarshalWithOptions([]byte(src), &v,
			yaml.WithResourceLimit(yaml.ResourceLimit{MaxAliasDepth: 100, MaxLogicalNodes: 100000}),
		); !yaml.IsAliasCycleError(err) {
			t.Fatalf("expected decode to report alias cycle, got %v", err)
		}
	}
}

func TestResourceLimit_IsolatedPerDecoderAndBackwardCompatible(t *testing.T) {
	src := exponentialBomb(20)

	// Default decoder without a limit still decodes the amplified document.
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("default decoder must remain unrestricted, got %v", err)
	}

	// Two decoders do not share budgets: a limited decoder and an unlimited
	// one are independent.
	limited := yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimit(yaml.ResourceLimit{
		MaxLogicalNodes: 100,
	}))
	unlimited := yaml.NewDecoder(strings.NewReader(src))

	var lv any
	if err := limited.Decode(&lv); !yaml.IsResourceLimitError(err) {
		t.Fatalf("limited decoder should reject, got %v", err)
	}
	var uv any
	if err := unlimited.Decode(&uv); err != nil {
		t.Fatalf("unlimited decoder should decode, got %v", err)
	}

	// Zero-valued limit imposes nothing.
	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.CheckResourceLimit(yaml.ResourceLimit{}, fileNodes(f)...); err != nil {
		t.Fatalf("zero limit must be a no-op, got %v", err)
	}
}

func TestResourceLimit_ASTCheckDoesNotDecode(t *testing.T) {
	// Syntax-error-free document that would only fail at runtime for a
	// strict target; the check operates purely on the AST.
	src := "a: &a 1\nb: *a\nc: *a\n"
	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimit(yaml.ResourceLimit{
		MaxSyntaxNodes:  100,
		MaxAliasDepth:   5,
		MaxLogicalNodes: 100,
	}))
	if err := dec.CheckResourceLimit(fileNodes(f)...); err != nil {
		t.Fatalf("check on parsed AST failed: %v", err)
	}
}

func TestResourceLimit_MergeKeyTagsAndStreams(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "merge key",
			src:  "defaults: &d\n  a: 1\n  b: 2\nx:\n  <<: *d\n  c: 3\n",
		},
		{
			name: "tag wrapping anchor",
			src:  "v: !!str &a 1\nw: *a\nx: *a\n",
		},
		{
			name: "block sequence aliases",
			src:  "- &a 1\n- *a\n- *a\n",
		},
		{
			name: "literal anchor",
			src:  "p: &p |\n  hello\n  world\nq: *p\n",
		},
	}
	limit := yaml.ResourceLimit{
		MaxSyntaxNodes:  1000,
		MaxAliasDepth:   5,
		MaxLogicalNodes: 1000,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseBytes([]byte(tc.src), 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := yaml.CheckResourceLimit(limit, fileNodes(f)...); err != nil {
				t.Fatalf("check failed for valid document: %v", err)
			}
			var v any
			if err := yaml.UnmarshalWithOptions([]byte(tc.src), &v, yaml.WithResourceLimit(limit)); err != nil {
				t.Fatalf("decode failed for valid document: %v", err)
			}
		})
	}

	// Multi-document streams are bounded across every document.
	src := "a: &a 1\n---\nb: *a\nc: *a\n"
	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.CheckResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: 3}, fileNodes(f)...); err == nil {
		t.Fatal("expected logical violation across documents")
	}
}

func TestResourceLimit_NodeAPI(t *testing.T) {
	src := "a: &a 1\nb: *a\n"
	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Checking a single node (not the whole file) must work as well.
	if err := yaml.CheckResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: 100}, f.Docs[0].Body); err != nil {
		t.Fatalf("node-level check failed: %v", err)
	}
}

// countLogicalValue counts the nodes produced by decoding into any, mirroring
// the estimator's definition: one node per scalar/container element.
func countLogicalValue(v any) uint64 {
	switch x := v.(type) {
	case nil:
		return 1
	case []any:
		var n uint64 = 1
		for _, e := range x {
			n += countLogicalValue(e)
		}
		return n
	case map[string]any:
		var n uint64 = 1
		for _, e := range x {
			n += countLogicalValue(e)
		}
		return n
	default:
		return 1
	}
}

func TestResourceLimit_LogicalEstimateMatchesDecodedValue(t *testing.T) {
	src := strings.Join([]string{
		"scalar: &s 1",
		"seq: &q [1, 2, *s]",
		"map: &m",
		"  x: *s",
		"  y: *q",
		"items:",
		"  - *q",
		"  - *m",
		"  - *s",
		"nested: [[*s, *s], {z: *m}]",
	}, "\n")

	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	actual := countLogicalValue(v)

	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	// An exactly-fitting budget must pass; one node fewer must fail.
	if err := yaml.CheckResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: actual}, fileNodes(f)...); err != nil {
		t.Fatalf("expected exact budget %d to pass, got %v", actual, err)
	}
	err = yaml.CheckResourceLimit(yaml.ResourceLimit{MaxLogicalNodes: actual - 1}, fileNodes(f)...)
	var rle *yaml.ResourceLimitError
	if err == nil || !errors.As(err, &rle) {
		t.Fatalf("expected logical violation at budget %d (actual %d), got %v", actual-1, actual, err)
	}
	if rle.Kind != "logical nodes" {
		t.Fatalf("unexpected kind %q", rle.Kind)
	}
	t.Logf("estimated logical nodes = %d matches decoded value", actual)
}

func TestResourceLimit_DeepLinearAliasChain(t *testing.T) {
	// A long, non-amplifying chain: each anchor only adds one scalar.
	const depth = 5000
	var b strings.Builder
	b.WriteString("a0: &a0 0\n")
	for i := 1; i < depth; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [*a%d]\n", i, i, i-1)
	}
	fmt.Fprintf(&b, "root: *a%d\n", depth-1)
	src := b.String()

	f, err := parser.ParseBytes([]byte(src), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The syntax graph is small despite the long reference chain.
	if got := countNodes(f.Docs[0]); got > uint64(depth*8) {
		t.Fatalf("syntax graph unexpectedly large: %d", got)
	}

	// Depth budget rejects with the chain reported.
	err = yaml.CheckResourceLimit(yaml.ResourceLimit{MaxAliasDepth: 100}, fileNodes(f)...)
	var rle *yaml.ResourceLimitError
	if err == nil || !errors.As(err, &rle) {
		t.Fatalf("expected alias depth violation, got %v", err)
	}
	if rle.Kind != "alias depth" {
		t.Fatalf("expected alias depth kind, got %q", rle.Kind)
	}
	if len(rle.AliasPath) != 101 {
		t.Fatalf("expected chain length 101, got %d", len(rle.AliasPath))
	}

	// No depth limit but a sufficient logical budget accepts the chain.
	limit := yaml.ResourceLimit{
		MaxSyntaxNodes:  uint64(depth * 10),
		MaxLogicalNodes: uint64(depth*depth/2 + depth*10),
	}
	if err := yaml.CheckResourceLimit(limit, fileNodes(f)...); err != nil {
		t.Fatalf("expected deep chain within budgets, got %v", err)
	}
}
