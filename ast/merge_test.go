package ast_test

import (
	"errors"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

func parseToMapping(t *testing.T, yaml string, key string) (*ast.File, *ast.MappingNode) {
	t.Helper()
	file, err := parser.ParseBytes([]byte(yaml), parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}
	if len(file.Docs) == 0 {
		t.Fatal("no documents found")
	}
	body, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		t.Fatalf("document body is %T, expected *ast.MappingNode", file.Docs[0].Body)
	}
	for _, value := range body.Values {
		if value.Key.GetToken().Value == key {
			mapping, ok := value.Value.(*ast.MappingNode)
			if !ok {
				t.Fatalf("value of %q is %T, expected *ast.MappingNode", key, value.Value)
			}
			return file, mapping
		}
	}
	t.Fatalf("key %q not found", key)
	return nil, nil
}

func explanationByKey(t *testing.T, explanations []*ast.MergeExplanation, key string) *ast.MergeExplanation {
	t.Helper()
	for _, explanation := range explanations {
		if explanation.Key == key {
			return explanation
		}
	}
	t.Fatalf("no explanation for key %q", key)
	return nil
}

func TestMergeExplainer_PriorityAndLocalOverride(t *testing.T) {
	yaml := `
base1: &base1
  name: from-base1
  timeout: 10
base2: &base2
  name: from-base2
  retries: 3
service:
  <<: [*base1, *base2]
  timeout: 30
`
	file, service := parseToMapping(t, yaml, "service")
	explanations, err := ast.NewMergeExplainer(file).Explain(service)
	if err != nil {
		t.Fatalf("failed to explain: %v", err)
	}
	if len(explanations) != 3 {
		t.Fatalf("expected 3 effective keys, got %d", len(explanations))
	}

	// A local explicit key overrides the merged candidate.
	timeout := explanationByKey(t, explanations, "timeout")
	if timeout.Value.GetToken().Value != "30" {
		t.Fatalf("expected effective timeout 30, got %s", timeout.Value.GetToken().Value)
	}
	if timeout.Source.Kind != ast.LocalMergeSource {
		t.Fatalf("expected local source, got %s", timeout.Source.Kind)
	}
	if got := timeout.Source.Position().Line; got != 10 {
		t.Fatalf("expected local source at line 10, got %d", got)
	}
	if len(timeout.Overridden) != 1 {
		t.Fatalf("expected 1 overridden timeout candidate, got %d", len(timeout.Overridden))
	}
	overriddenTimeout := timeout.Overridden[0]
	if overriddenTimeout.Kind != ast.MergedMergeSource {
		t.Fatalf("expected merged source, got %s", overriddenTimeout.Kind)
	}
	if got := overriddenTimeout.Entry.Value.GetToken().Value; got != "10" {
		t.Fatalf("expected overridden timeout 10 to remain reachable, got %s", got)
	}
	if got := overriddenTimeout.Position().Line; got != 4 {
		t.Fatalf("expected overridden timeout at line 4, got %d", got)
	}

	// The earlier merge sequence entry wins over the later one.
	name := explanationByKey(t, explanations, "name")
	if name.Value.GetToken().Value != "from-base1" {
		t.Fatalf("expected effective name from-base1, got %s", name.Value.GetToken().Value)
	}
	if name.Source.Kind != ast.MergedMergeSource {
		t.Fatalf("expected merged source, got %s", name.Source.Kind)
	}
	if len(name.Overridden) != 1 {
		t.Fatalf("expected 1 overridden name candidate, got %d", len(name.Overridden))
	}
	if got := name.Overridden[0].Entry.Value.GetToken().Value; got != "from-base2" {
		t.Fatalf("expected overridden name from-base2 to remain reachable, got %s", got)
	}

	// The source chain records both the alias reference and the anchor definition.
	if len(name.Source.Steps) != 1 {
		t.Fatalf("expected 1 resolution step, got %d", len(name.Source.Steps))
	}
	step := name.Source.Steps[0]
	if got := step.Alias.GetToken().Position.Line; got != 9 {
		t.Fatalf("expected alias reference at line 9, got %d", got)
	}
	if got := step.Alias.Value.GetToken().Value; got != "base1" {
		t.Fatalf("expected alias *base1, got *%s", got)
	}
	if got := step.Anchor.GetToken().Position.Line; got != 2 {
		t.Fatalf("expected anchor definition at line 2, got %d", got)
	}
	if got := step.Anchor.Name.GetToken().Value; got != "base1" {
		t.Fatalf("expected anchor &base1, got &%s", got)
	}

	// The explanation references the original nodes; it does not copy the subtree.
	base1Anchor := file.Docs[0].Body.(*ast.MappingNode).Values[0].Value.(*ast.AnchorNode)
	base1Name := base1Anchor.Value.(*ast.MappingNode).Values[0]
	if name.Source.Entry != base1Name {
		t.Fatal("expected explanation to reference the original anchor entry node")
	}

	// A key contributed only by the later merge entry is still effective.
	retries := explanationByKey(t, explanations, "retries")
	if retries.Value.GetToken().Value != "3" {
		t.Fatalf("expected effective retries 3, got %s", retries.Value.GetToken().Value)
	}
	if len(retries.Overridden) != 0 {
		t.Fatalf("expected no overridden retries candidates, got %d", len(retries.Overridden))
	}
}

func TestMergeExplainer_MergeSequenceOrderSwap(t *testing.T) {
	yaml := `
base1: &base1
  name: from-base1
base2: &base2
  name: from-base2
service:
  <<: [*base1, *base2]
`
	file, service := parseToMapping(t, yaml, "service")
	explanations, err := ast.NewMergeExplainer(file).Explain(service)
	if err != nil {
		t.Fatalf("failed to explain: %v", err)
	}
	name := explanationByKey(t, explanations, "name")
	if got := name.Value.GetToken().Value; got != "from-base1" {
		t.Fatalf("expected effective name from-base1, got %s", got)
	}
	if got := name.Overridden[0].Entry.Value.GetToken().Value; got != "from-base2" {
		t.Fatalf("expected overridden name from-base2, got %s", got)
	}

	swapped := `
base1: &base1
  name: from-base1
base2: &base2
  name: from-base2
service:
  <<: [*base2, *base1]
`
	swappedFile, swappedService := parseToMapping(t, swapped, "service")
	swappedExplanations, err := ast.NewMergeExplainer(swappedFile).Explain(swappedService)
	if err != nil {
		t.Fatalf("failed to explain: %v", err)
	}
	swappedName := explanationByKey(t, swappedExplanations, "name")
	if got := swappedName.Value.GetToken().Value; got != "from-base2" {
		t.Fatalf("expected effective name from-base2 after swap, got %s", got)
	}
	if got := swappedName.Overridden[0].Entry.Value.GetToken().Value; got != "from-base1" {
		t.Fatalf("expected overridden name from-base1 after swap, got %s", got)
	}
}

func TestMergeExplainer_NestedMergeChain(t *testing.T) {
	yaml := `
base: &base
  a: 1
mid: &mid
  <<: *base
  b: 2
top:
  <<: *mid
`
	file, top := parseToMapping(t, yaml, "top")
	explanations, err := ast.NewMergeExplainer(file).Explain(top)
	if err != nil {
		t.Fatalf("failed to explain: %v", err)
	}
	a := explanationByKey(t, explanations, "a")
	if len(a.Source.Steps) != 2 {
		t.Fatalf("expected 2 resolution steps for nested merge, got %d", len(a.Source.Steps))
	}
	if got := a.Source.Steps[0].Anchor.Name.GetToken().Value; got != "mid" {
		t.Fatalf("expected first hop &mid, got &%s", got)
	}
	if got := a.Source.Steps[1].Anchor.Name.GetToken().Value; got != "base" {
		t.Fatalf("expected second hop &base, got &%s", got)
	}
	b := explanationByKey(t, explanations, "b")
	if len(b.Source.Steps) != 1 {
		t.Fatalf("expected 1 resolution step, got %d", len(b.Source.Steps))
	}
}

func TestMergeExplainer_MergeCycle(t *testing.T) {
	yaml := `
a: &a
  <<: *b
  x: 1
b: &b
  <<: *a
  y: 2
c:
  <<: *a
`
	file, c := parseToMapping(t, yaml, "c")
	_, err := ast.NewMergeExplainer(file).Explain(c)
	if err == nil {
		t.Fatal("expected a merge cycle error")
	}
	var cycleErr *ast.MergeCycleError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("expected *ast.MergeCycleError, got %T: %v", err, err)
	}
	if len(cycleErr.Chain) != 3 {
		t.Fatalf("expected a 3-hop cycle chain, got %d", len(cycleErr.Chain))
	}
	names := []string{
		cycleErr.Chain[0].Anchor.Name.GetToken().Value,
		cycleErr.Chain[1].Anchor.Name.GetToken().Value,
		cycleErr.Chain[2].Anchor.Name.GetToken().Value,
	}
	if names[0] != "a" || names[1] != "b" || names[2] != "a" {
		t.Fatalf("unexpected cycle chain %v", names)
	}
	if cycleErr.Chain[2].Alias.GetToken().Position.Line != 6 {
		t.Fatalf("expected the closing alias reference at line 6, got %d", cycleErr.Chain[2].Alias.GetToken().Position.Line)
	}
}

func TestMergeExplainer_SharedAliasKeepsOwnReferencePath(t *testing.T) {
	yaml := `
defaults: &defaults
  mode: auto
svc1:
  <<: *defaults
svc2:
  <<: *defaults
`
	file, svc1 := parseToMapping(t, yaml, "svc1")
	explainer := ast.NewMergeExplainer(file)
	var svc2 *ast.MappingNode
	for _, value := range file.Docs[0].Body.(*ast.MappingNode).Values {
		if value.Key.GetToken().Value == "svc2" {
			svc2 = value.Value.(*ast.MappingNode)
		}
	}
	if svc2 == nil {
		t.Fatal("svc2 not found")
	}

	mode1 := explanationByKey(t, mustExplain(t, explainer, svc1), "mode")
	mode2 := explanationByKey(t, mustExplain(t, explainer, svc2), "mode")

	alias1 := mode1.Source.Steps[0].Alias
	alias2 := mode2.Source.Steps[0].Alias
	if alias1 == alias2 {
		t.Fatal("expected each use site of the shared alias to keep its own reference node")
	}
	if alias1.GetToken().Position.Line != 5 {
		t.Fatalf("expected svc1 alias reference at line 5, got %d", alias1.GetToken().Position.Line)
	}
	if alias2.GetToken().Position.Line != 7 {
		t.Fatalf("expected svc2 alias reference at line 7, got %d", alias2.GetToken().Position.Line)
	}
	if mode1.Source.Steps[0].Anchor != mode2.Source.Steps[0].Anchor {
		t.Fatal("expected both references to resolve to the same anchor definition")
	}
}

func mustExplain(t *testing.T, explainer *ast.MergeExplainer, m *ast.MappingNode) []*ast.MergeExplanation {
	t.Helper()
	explanations, err := explainer.Explain(m)
	if err != nil {
		t.Fatalf("failed to explain: %v", err)
	}
	return explanations
}
