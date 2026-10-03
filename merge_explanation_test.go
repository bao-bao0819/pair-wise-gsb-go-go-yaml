package yaml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

func TestExplainMergeSequencePrecedence(t *testing.T) {
	first := `
a: &a
  x: first
  y: first-only
b: &b
  x: second
  z: second-only
result:
  <<: [*a, *b]
  w: local
`
	second := strings.Replace(first, "<<: [*a, *b]", "<<: [*b, *a]", 1)

	firstFile, err := parser.ParseBytes([]byte(first), 0)
	if err != nil {
		t.Fatal(err)
	}
	secondFile, err := parser.ParseBytes([]byte(second), 0)
	if err != nil {
		t.Fatal(err)
	}
	firstTarget := targetMapping(t, firstFile)
	secondTarget := targetMapping(t, secondFile)

	firstExplanation, err := ast.NewMergeResolver(firstFile).Explain(firstTarget)
	if err != nil {
		t.Fatal(err)
	}
	secondExplanation, err := ast.NewMergeResolver(secondFile).Explain(secondTarget)
	if err != nil {
		t.Fatal(err)
	}

	assertSelected := func(t *testing.T, explanation *ast.MergeExplanation, key, want string) {
		t.Helper()
		item := explanation.Lookup(key)
		if item == nil {
			t.Fatalf("missing explanation for %q", key)
		}
		if got := item.Value.(*ast.StringNode).Value; got != want {
			t.Fatalf("key %q = %q, want %q", key, got, want)
		}
	}
	assertSelected(t, firstExplanation, "x", "first")
	assertSelected(t, firstExplanation, "y", "first-only")
	assertSelected(t, firstExplanation, "z", "second-only")
	assertSelected(t, firstExplanation, "w", "local")
	assertSelected(t, secondExplanation, "x", "second")
	assertSelected(t, secondExplanation, "y", "first-only")
	assertSelected(t, secondExplanation, "z", "second-only")
	assertSelected(t, secondExplanation, "w", "local")
	assertKeys := func(t *testing.T, explanation *ast.MergeExplanation, want ...string) {
		t.Helper()
		if len(explanation.Keys) != len(want) {
			t.Fatalf("len(keys) = %d, want %d (%v)", len(explanation.Keys), len(want), want)
		}
		for i, key := range want {
			if got := explanation.Keys[i].(*ast.StringNode).Value; got != key {
				t.Fatalf("keys[%d] = %q, want %q", i, got, key)
			}
		}
	}
	assertKeys(t, firstExplanation, "w", "x", "y", "z")
	assertKeys(t, secondExplanation, "w", "x", "z", "y")

	x := firstExplanation.Lookup("x")
	if got := x.Value.(*ast.StringNode).Value; got != "first" {
		t.Fatalf("x = %q, want first", got)
	}
	if len(x.Overridden) != 1 {
		t.Fatalf("len(x.Overridden) = %d, want 1", len(x.Overridden))
	}
	if got := x.Overridden[0].Value.(*ast.StringNode).Value; got != "second" {
		t.Fatalf("overridden x = %q, want second", got)
	}
	if len(x.Selected.Chain) != 1 || x.Selected.Chain[0].Alias == nil || x.Selected.Chain[0].Anchor == nil {
		t.Fatalf("selected chain = %#v, want one alias/anchor hop", x.Selected.Chain)
	}
	if x.Selected.Chain[0].SequenceIndex == nil || *x.Selected.Chain[0].SequenceIndex != 0 {
		t.Fatalf("selected sequence index = %v, want 0", x.Selected.Chain[0].SequenceIndex)
	}
	if x.Selected.Chain[0].Alias.Value.(*ast.StringNode).Value != "a" {
		t.Fatal("selected candidate does not reference a")
	}
	if got := x.Overridden[0].Chain[0].Alias.Value.(*ast.StringNode).Value; got != "b" {
		t.Fatalf("overridden chain alias = %q, want b", got)
	}
	if x.Overridden[0].Chain[0].SequenceIndex == nil || *x.Overridden[0].Chain[0].SequenceIndex != 1 {
		t.Fatalf("overridden sequence index = %v, want 1", x.Overridden[0].Chain[0].SequenceIndex)
	}
	if x.Selected.ValueLocation.Line != 3 || x.Overridden[0].ValueLocation.Line != 6 {
		t.Fatalf("candidate value lines = %d/%d, want 3/6", x.Selected.ValueLocation.Line, x.Overridden[0].ValueLocation.Line)
	}

	swapped := secondExplanation.Lookup("x")
	if got := swapped.Value.(*ast.StringNode).Value; got != "second" {
		t.Fatalf("swapped x = %q, want second", got)
	}
	if len(swapped.Overridden) != 1 {
		t.Fatalf("len(swapped.Overridden) = %d, want 1", len(swapped.Overridden))
	}
	if got := swapped.Overridden[0].Value.(*ast.StringNode).Value; got != "first" {
		t.Fatalf("swapped overridden x = %q, want first", got)
	}
	if swapped.Selected.Chain[0].SequenceIndex == nil || *swapped.Selected.Chain[0].SequenceIndex != 0 {
		t.Fatalf("swapped sequence index = %v, want 0", swapped.Selected.Chain[0].SequenceIndex)
	}
}

func TestExplainMergeLocalOverride(t *testing.T) {
	source := `
a: &a
  x: first
b: &b
  x: second
result:
  <<: [*a, *b]
  x: local
`
	file, err := parser.ParseBytes([]byte(source), 0)
	if err != nil {
		t.Fatal(err)
	}
	explanation, err := ast.NewMergeResolver(file).Explain(targetMapping(t, file))
	if err != nil {
		t.Fatal(err)
	}
	x := explanation.Lookup("x")
	if got := x.Value.(*ast.StringNode).Value; got != "local" {
		t.Fatalf("x = %q, want local", got)
	}
	if len(x.Selected.Chain) != 0 {
		t.Fatalf("selected chain length = %d, want 0", len(x.Selected.Chain))
	}
	if len(x.Overridden) != 2 {
		t.Fatalf("len(x.Overridden) = %d, want 2", len(x.Overridden))
	}
	overridden := map[string]bool{}
	for _, candidate := range x.Overridden {
		overridden[candidate.Value.(*ast.StringNode).Value] = true
	}
	if !overridden["first"] || !overridden["second"] {
		t.Fatalf("overridden values = %#v, want first and second", x.Overridden)
	}
}

func TestExplainMergeKeepsSharedAliasReferencePaths(t *testing.T) {
	source := `
base: &base
  shared: value
a: &a
  <<: *base
  a-only: yes
b: &b
  <<: *base
  b-only: yes
result:
  <<: [*a, *b]
`
	file, err := parser.ParseBytes([]byte(source), 0)
	if err != nil {
		t.Fatal(err)
	}
	explanation, err := ast.NewMergeResolver(file).Explain(targetMapping(t, file))
	if err != nil {
		t.Fatal(err)
	}
	shared := explanation.Lookup("shared")
	if shared == nil {
		t.Fatal("missing shared key")
	}
	if len(shared.Selected.Chain) != 2 {
		t.Fatalf("selected chain length = %d, want 2", len(shared.Selected.Chain))
	}
	if got := shared.Selected.Chain[0].Alias.Value.(*ast.StringNode).Value; got != "a" {
		t.Fatalf("first selected alias = %q, want a", got)
	}
	if got := shared.Selected.Chain[1].Alias.Value.(*ast.StringNode).Value; got != "base" {
		t.Fatalf("second selected alias = %q, want base", got)
	}
	if len(shared.Overridden) != 1 || len(shared.Overridden[0].Chain) != 2 {
		t.Fatalf("overridden chains = %#v, want one two-hop chain", shared.Overridden)
	}
	overridden := shared.Overridden[0].Chain
	if got := overridden[0].Alias.Value.(*ast.StringNode).Value; got != "b" {
		t.Fatalf("first overridden alias = %q, want b", got)
	}
	if got := overridden[1].Alias.Value.(*ast.StringNode).Value; got != "base" {
		t.Fatalf("second overridden alias = %q, want base", got)
	}
	if shared.Selected.Chain[1].Alias == overridden[1].Alias {
		t.Fatal("nested alias references were not distinguished")
	}
}

func TestExplainMergeNestedCycle(t *testing.T) {
	source := `
a: &a
  <<: *a
  x: from-a
result:
  <<: *a
`
	file, err := parser.ParseBytes([]byte(source), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ast.NewMergeResolver(file).Explain(targetMapping(t, file))
	var cycleErr *ast.MergeCycleError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("err = %#v, want *MergeCycleError", err)
	}
	if len(cycleErr.Chain) != 2 {
		t.Fatalf("cycle chain length = %d, want 2", len(cycleErr.Chain))
	}
	if got := cycleErr.Chain[0].Alias.Value.(*ast.StringNode).Value; got != "a" {
		t.Fatalf("first cycle alias = %q, want a", got)
	}
	if got := cycleErr.Chain[1].Alias.Value.(*ast.StringNode).Value; got != "a" {
		t.Fatalf("second cycle alias = %q, want a", got)
	}
}

func TestExplainMergeInvalidValue(t *testing.T) {
	source := "result:\n  <<: 42\n"
	file, err := parser.ParseBytes([]byte(source), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ast.ExplainMerge(targetMapping(t, file))
	if !errors.Is(err, ast.ErrInvalidMergeValue) {
		t.Fatalf("err = %v, want ast.ErrInvalidMergeValue", err)
	}
}

func TestExplainMergeDoesNotMutateAST(t *testing.T) {
	source := `
a: &a
  x: first
b: &b
  x: second
result:
  <<: [*a, *b]
`
	file, err := parser.ParseBytes([]byte(source), 0)
	if err != nil {
		t.Fatal(err)
	}
	before := file.String()
	if _, err := ast.NewMergeResolver(file).Explain(targetMapping(t, file)); err != nil {
		t.Fatal(err)
	}
	if after := file.String(); after != before {
		t.Fatalf("AST changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func targetMapping(t *testing.T, file *ast.File) *ast.MappingNode {
	t.Helper()
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		t.Fatalf("root = %T, want *ast.MappingNode", file.Docs[0].Body)
	}
	for _, entry := range root.Values {
		if key, ok := entry.Key.(*ast.StringNode); ok && key.Value == "result" {
			target, ok := entry.Value.(*ast.MappingNode)
			if !ok {
				t.Fatalf("result = %T, want *ast.MappingNode", entry.Value)
			}
			return target
		}
	}
	t.Fatal("result mapping not found")
	return nil
}
