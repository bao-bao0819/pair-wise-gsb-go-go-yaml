package ast

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml/token"
)

// MergeSourceKind describes where an effective map entry comes from.
type MergeSourceKind int

const (
	// LocalMergeSource indicates a key written explicitly in the mapping itself.
	LocalMergeSource MergeSourceKind = iota
	// MergedMergeSource indicates a key contributed through a `<<` merge key.
	MergedMergeSource
)

// String returns the string representation of the merge source kind.
func (k MergeSourceKind) String() string {
	switch k {
	case LocalMergeSource:
		return "Local"
	case MergedMergeSource:
		return "Merged"
	}
	return "Unknown"
}

// MergeSourceStep is a single hop of an alias resolution chain.
// It records both the alias reference point and the anchor definition point
// the alias resolves to.
type MergeSourceStep struct {
	// Alias is the alias node at the reference (use) site.
	Alias *AliasNode
	// Anchor is the anchor definition the alias resolves to.
	Anchor *AnchorNode
}

// MergeSource describes a single candidate origin of an effective key.
// The node values are referenced as-is; nothing is copied or expanded.
type MergeSource struct {
	// Kind tells whether the entry is local to the explained mapping
	// or was contributed through a merge key.
	Kind MergeSourceKind
	// Steps is the alias resolution chain from the merge key to the anchor
	// that defines the mapping containing the entry. It is empty for local
	// entries. Each use site of a shared alias keeps its own steps.
	Steps []MergeSourceStep
	// Entry is the key/value entry that provides the candidate value.
	Entry *MappingValueNode
}

// Position returns the source position of the key that provides the value.
func (s MergeSource) Position() *token.Position {
	return s.Entry.Key.GetToken().Position
}

// MergeExplanation explains how one effective key of a mapping is composed.
type MergeExplanation struct {
	// Key is the effective key name.
	Key string
	// Value is the final effective value node.
	Value Node
	// Source is the winning source of the effective value.
	Source MergeSource
	// Overridden lists the candidate sources that lost, highest precedence
	// first. Overridden values remain reachable through their Entry field.
	Overridden []MergeSource
}

// MergeCycleError is returned when nested merge keys form a cycle
// through anchors and aliases.
type MergeCycleError struct {
	// Chain is the alias resolution chain that leads back to an anchor
	// that is already being resolved.
	Chain []MergeSourceStep
}

// Error implements the error interface.
func (e *MergeCycleError) Error() string {
	names := make([]string, 0, len(e.Chain))
	for _, step := range e.Chain {
		names = append(names, step.Anchor.Name.GetToken().Value)
	}
	return fmt.Sprintf("yaml: merge cycle detected: %s", strings.Join(names, " -> "))
}

// MergeExplainer explains the effective keys of mappings that use the
// YAML merge key ( `<<` ). It follows the YAML merge semantics: keys
// written explicitly in a mapping always override merged keys, and in a
// merge sequence ( `<<: [*a, *b]` ) earlier entries override later ones.
// It never mutates the AST, so decoding results and comment attribution
// are unaffected.
type MergeExplainer struct {
	anchors map[string]*AnchorNode
}

// NewMergeExplainer collects the anchor definitions under root and returns
// a MergeExplainer. root may be a *File, a *DocumentNode, or any other Node.
func NewMergeExplainer(root interface{}) *MergeExplainer {
	e := &MergeExplainer{anchors: map[string]*AnchorNode{}}
	collect := func(node Node) {
		walkMergeAnchors(node, e.anchors)
	}
	switch n := root.(type) {
	case *File:
		for _, doc := range n.Docs {
			collect(doc)
		}
	case Node:
		collect(n)
	}
	return e
}

func walkMergeAnchors(node Node, anchors map[string]*AnchorNode) {
	switch n := node.(type) {
	case *DocumentNode:
		if n.Body != nil {
			walkMergeAnchors(n.Body, anchors)
		}
	case *MappingNode:
		for _, value := range n.Values {
			walkMergeAnchors(value, anchors)
		}
	case *MappingValueNode:
		if n.Key != nil {
			walkMergeAnchors(n.Key, anchors)
		}
		if n.Value != nil {
			walkMergeAnchors(n.Value, anchors)
		}
	case *SequenceNode:
		for _, value := range n.Values {
			walkMergeAnchors(value, anchors)
		}
	case *SequenceEntryNode:
		if n.Value != nil {
			walkMergeAnchors(n.Value, anchors)
		}
	case *AnchorNode:
		if n.Name != nil {
			anchors[n.Name.GetToken().Value] = n
		}
		if n.Value != nil {
			walkMergeAnchors(n.Value, anchors)
		}
	case *TagNode:
		if n.Value != nil {
			walkMergeAnchors(n.Value, anchors)
		}
	}
}

// Explain resolves the merge keys of m and returns one explanation per
// effective key, in order of first appearance. A nested merge that forms
// a cycle results in a *MergeCycleError carrying the resolution chain.
func (e *MergeExplainer) Explain(m *MappingNode) ([]*MergeExplanation, error) {
	entries, err := e.effectiveEntries(m, nil, map[string]bool{})
	if err != nil {
		return nil, err
	}
	var explanations []*MergeExplanation
	byKey := map[string]*MergeExplanation{}
	for _, entry := range entries {
		if explanation, exists := byKey[entry.key]; exists {
			explanation.Overridden = append(explanation.Overridden, entry.source)
			continue
		}
		explanation := &MergeExplanation{
			Key:    entry.key,
			Value:  entry.source.Entry.Value,
			Source: entry.source,
		}
		byKey[entry.key] = explanation
		explanations = append(explanations, explanation)
	}
	return explanations, nil
}

type mergeEntry struct {
	key    string
	source MergeSource
}

// effectiveEntries returns the candidate entries of m ordered by precedence
// (highest first): explicit local keys first, then merge key contributions
// in document order. steps is the alias resolution chain that led to m.
func (e *MergeExplainer) effectiveEntries(m *MappingNode, steps []MergeSourceStep, resolving map[string]bool) ([]mergeEntry, error) {
	var locals []mergeEntry
	var merged []mergeEntry
	for _, value := range m.Values {
		if value.Key == nil || value.Value == nil {
			continue
		}
		if !value.Key.IsMergeKey() {
			kind := MergedMergeSource
			if len(steps) == 0 {
				kind = LocalMergeSource
			}
			locals = append(locals, mergeEntry{
				key: value.Key.GetToken().Value,
				source: MergeSource{
					Kind:  kind,
					Steps: steps,
					Entry: value,
				},
			})
			continue
		}
		entries, err := e.mergeValueEntries(value.Value, steps, resolving)
		if err != nil {
			return nil, err
		}
		merged = append(merged, entries...)
	}
	return append(locals, merged...), nil
}

// mergeValueEntries expands a merge key value: a single alias, an inline
// anchor, or a sequence of aliases in which earlier entries take precedence.
func (e *MergeExplainer) mergeValueEntries(value Node, steps []MergeSourceStep, resolving map[string]bool) ([]mergeEntry, error) {
	switch n := value.(type) {
	case *AliasNode:
		return e.aliasEntries(n, steps, resolving)
	case *AnchorNode:
		return e.anchorEntries(n, steps, resolving)
	case *SequenceNode:
		var entries []mergeEntry
		for _, elem := range n.Values {
			sub, err := e.mergeValueEntries(elem, steps, resolving)
			if err != nil {
				return nil, err
			}
			entries = append(entries, sub...)
		}
		return entries, nil
	}
	return nil, fmt.Errorf("yaml: unsupported merge key value %s", value.GetToken().Value)
}

func (e *MergeExplainer) aliasEntries(alias *AliasNode, steps []MergeSourceStep, resolving map[string]bool) ([]mergeEntry, error) {
	name := alias.Value.GetToken().Value
	anchor, exists := e.anchors[name]
	if !exists {
		return nil, fmt.Errorf("yaml: cannot find anchor by alias name %s", name)
	}
	steps = append(append([]MergeSourceStep{}, steps...), MergeSourceStep{Alias: alias, Anchor: anchor})
	if resolving[name] {
		return nil, &MergeCycleError{Chain: steps}
	}
	return e.anchorEntries(anchor, steps, resolving)
}

func (e *MergeExplainer) anchorEntries(anchor *AnchorNode, steps []MergeSourceStep, resolving map[string]bool) ([]mergeEntry, error) {
	name := anchor.Name.GetToken().Value
	mapping, ok := anchor.Value.(*MappingNode)
	if !ok {
		return nil, fmt.Errorf("yaml: anchor %s does not define a mapping", name)
	}
	if resolving[name] {
		return nil, &MergeCycleError{Chain: steps}
	}
	resolving[name] = true
	defer delete(resolving, name)
	return e.effectiveEntries(mapping, steps, resolving)
}
