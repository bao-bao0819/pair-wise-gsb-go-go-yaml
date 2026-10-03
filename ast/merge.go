package ast

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml/token"
)

var (
	ErrUndefinedMergeAlias = fmt.Errorf("undefined merge alias")
	ErrInvalidMergeValue   = fmt.Errorf("invalid merge value")
)

type SourceLocation struct {
	Node     Node
	Position *token.Position
	Line     int
	Column   int
	Offset   int
}

// MergeHop describes one alias/anchor traversal in a merge chain.
type MergeHop struct {
	SequenceIndex    *int
	SequenceValue    Node
	SequenceLocation SourceLocation
	MergeKey         Node
	MergeKeyLocation SourceLocation
	Alias            *AliasNode
	AliasLocation    SourceLocation
	Anchor           *AnchorNode
	AnchorLocation   SourceLocation
}

// MergeCandidate is one effective or overridden source for a merged key.
type MergeCandidate struct {
	Key           Node
	Value         Node
	KeyLocation   SourceLocation
	ValueLocation SourceLocation
	Chain         []MergeHop
}

// MergeKeyExplanation explains every source candidate for one effective key.
type MergeKeyExplanation struct {
	Key        Node
	Value      Node
	Selected   MergeCandidate
	Overridden []MergeCandidate
}

// MergeExplanation contains effective keys and source explanations for one mapping.
type MergeExplanation struct {
	Mapping *MappingNode
	Keys    []Node
	Items   map[string]*MergeKeyExplanation
}

// ExplainMerge explains merge resolution for node using node as the anchor root.
func ExplainMerge(node interface{}) (*MergeExplanation, error) {
	return NewMergeResolver(node).Explain(node)
}

// MergeResolver explains mappings against anchors contained in root.
type MergeResolver struct {
	root interface{}
}

// NewMergeResolver creates a merge explanation resolver with the given anchor root.
func NewMergeResolver(root interface{}) *MergeResolver {
	return &MergeResolver{root: root}
}

// Explain explains the effective values of the specified mapping.
func (r *MergeResolver) Explain(node interface{}) (*MergeExplanation, error) {
	if r.root == nil {
		r.root = node
	}
	mapping, err := mergeMapping(node)
	if err != nil {
		return nil, err
	}
	resolver := &mergeResolver{anchors: map[string]*AnchorNode{}}
	resolver.collectAnchors(r.root)
	candidates, keyOrder, err := resolver.collectMapping(mapping, nil)
	if err != nil {
		return nil, err
	}
	items := make(map[string]*MergeKeyExplanation, len(candidates))
	for _, keyNode := range keyOrder {
		keyText, err := mergeKeyString(keyNode)
		if err != nil {
			return nil, err
		}
		sources := candidates[keyText]
		if len(sources) == 0 {
			continue
		}
		selected := sources[0]
		items[keyText] = &MergeKeyExplanation{
			Key:        selected.Key,
			Value:      selected.Value,
			Selected:   selected,
			Overridden: append([]MergeCandidate(nil), sources[1:]...),
		}
	}
	return &MergeExplanation{Mapping: mapping, Keys: keyOrder, Items: items}, nil
}

func (e *mergeResolver) collectMapping(mapNode MapNode, chain []MergeHop) (map[string][]MergeCandidate, []Node, error) {
	candidates := map[string][]MergeCandidate{}
	var keyOrder []Node
	var merges []*MappingValueNode
	var mergeResults []mappingCandidates

	iter := mapNode.MapRange()
	for iter.Next() {
		entry := iter.KeyValue()
		if entry.Key.IsMergeKey() {
			merges = append(merges, entry)
			continue
		}
		keyText, err := mergeKeyString(entry.Key)
		if err != nil {
			return nil, nil, err
		}
		candidate := MergeCandidate{
			Key:           entry.Key,
			Value:         entry.Value,
			KeyLocation:   sourceLocation(entry.Key),
			ValueLocation: sourceLocation(entry.Value),
			Chain:         cloneChain(chain),
		}
		if _, exists := candidates[keyText]; !exists {
			keyOrder = append(keyOrder, entry.Key)
		}
		candidates[keyText] = append(candidates[keyText], candidate)
	}

	for _, entry := range merges {
		mergeNodes, hops, err := e.resolveMergeValue(entry.Value, entry.Key, chain)
		if err != nil {
			return nil, nil, err
		}
		for i, merged := range mergeNodes {
			mergedCandidates, mergedOrder, err := e.collectMapping(merged, hops[i])
			if err != nil {
				return nil, nil, err
			}
			mergeResults = append(mergeResults, mappingCandidates{candidates: mergedCandidates, order: mergedOrder})
		}
	}
	for _, result := range mergeResults {
		for _, keyNode := range result.order {
			keyText, err := mergeKeyString(keyNode)
			if err != nil {
				return nil, nil, err
			}
			sources := result.candidates[keyText]
			if _, exists := candidates[keyText]; !exists && len(sources) > 0 {
				keyOrder = append(keyOrder, sources[0].Key)
			}
			candidates[keyText] = append(candidates[keyText], sources...)
		}
	}
	return candidates, keyOrder, nil
}

type mappingCandidates struct {
	candidates map[string][]MergeCandidate
	order      []Node
}

func (e *MergeExplanation) Lookup(key string) *MergeKeyExplanation {
	if e == nil {
		return nil
	}
	return e.Items[key]
}

type MergeCycleError struct {
	Name  string
	Chain []MergeHop
}

func (e *MergeCycleError) Error() string {
	names := []string{}
	for _, hop := range e.Chain {
		if hop.Alias != nil {
			names = append(names, hop.Alias.String())
		}
	}
	return fmt.Sprintf("cyclic merge reference: %s", strings.Join(names, " -> "))
}

type mergeResolver struct {
	anchors map[string]*AnchorNode
}

func (e *mergeResolver) collectAnchors(node interface{}) {
	switch n := node.(type) {
	case nil:
		return
	case *File:
		for _, doc := range n.Docs {
			e.collectAnchors(doc)
		}
	case *DocumentNode:
		if n != nil && n.Body != nil {
			e.collectAnchors(n.Body)
		}
	case *AnchorNode:
		if n != nil && n.Name != nil {
			if name, err := mergeScalarString(n.Name); err == nil {
				e.anchors[name] = n
			}
		}
		if n != nil && n.Value != nil {
			e.collectAnchors(n.Value)
		}
	case *MappingNode:
		for _, value := range n.Values {
			e.collectAnchors(value)
		}
	case *MappingValueNode:
		e.collectAnchors(n.Key)
		e.collectAnchors(n.Value)
	case *SequenceNode:
		for _, value := range n.Values {
			e.collectAnchors(value)
		}
	case *SequenceEntryNode:
		if n.Value != nil {
			e.collectAnchors(n.Value)
		}
	case *MappingKeyNode:
		if n.Value != nil {
			e.collectAnchors(n.Value)
		}
	case *TagNode:
		e.collectAnchors(n.Value)
	case *AliasNode:
		e.collectAnchors(n.Value)
	case *DirectiveNode:
		for _, value := range n.Values {
			e.collectAnchors(value)
		}
	}
}

func (e *mergeResolver) resolveMergeValue(node Node, mergeKey Node, chain []MergeHop) ([]MapNode, [][]MergeHop, error) {
	return e.resolveMergeValueWithSequence(node, mergeKey, -1, nil, chain)
}

func (e *mergeResolver) resolveMergeValueWithSequence(node Node, mergeKey Node, sequenceIndex int, sequenceValue Node, chain []MergeHop) ([]MapNode, [][]MergeHop, error) {
	var sequenceIndexPointer *int
	if sequenceIndex >= 0 {
		sequenceIndexPointer = &sequenceIndex
	}
	switch n := node.(type) {
	case nil:
		return nil, nil, fmt.Errorf("%w: nil", ErrInvalidMergeValue)
	case *AliasNode:
		if n == nil || n.Value == nil {
			return nil, nil, fmt.Errorf("%w: nil alias", ErrInvalidMergeValue)
		}
		name, err := mergeScalarString(n.Value)
		if err != nil {
			return nil, nil, err
		}
		anchor := e.anchors[name]
		if anchor == nil {
			return nil, nil, fmt.Errorf("%w: %q", ErrUndefinedMergeAlias, name)
		}
		hop := MergeHop{
			SequenceIndex:    sequenceIndexPointer,
			SequenceValue:    sequenceValue,
			SequenceLocation: sourceLocation(sequenceValue),
			MergeKey:         mergeKey,
			MergeKeyLocation: sourceLocation(mergeKey),
			Alias:            n,
			AliasLocation:    sourceLocation(n),
			Anchor:           anchor,
			AnchorLocation:   sourceLocation(anchor),
		}
		for _, previous := range chain {
			if previous.Anchor == anchor {
				return nil, nil, &MergeCycleError{Name: name, Chain: append(cloneChain(chain), hop)}
			}
		}
		mapping, err := mergeMapping(anchor.Value)
		if err != nil {
			return nil, nil, err
		}
		return []MapNode{mapping}, [][]MergeHop{append(cloneChain(chain), hop)}, nil
	case *SequenceNode:
		var maps []MapNode
		var chains [][]MergeHop
		for i, value := range n.Values {
			valueMaps, valueChains, err := e.resolveMergeValueWithSequence(value, mergeKey, i, value, chain)
			if err != nil {
				return nil, nil, err
			}
			maps = append(maps, valueMaps...)
			chains = append(chains, valueChains...)
		}
		return maps, chains, nil
	case *AnchorNode:
		mapping, err := mergeMapping(n.Value)
		if err != nil {
			return nil, nil, err
		}
		hop := MergeHop{
			SequenceIndex:    sequenceIndexPointer,
			SequenceValue:    sequenceValue,
			SequenceLocation: sourceLocation(sequenceValue),
			MergeKey:         mergeKey,
			MergeKeyLocation: sourceLocation(mergeKey),
			Anchor:           n,
			AnchorLocation:   sourceLocation(n),
		}
		return []MapNode{mapping}, [][]MergeHop{append(cloneChain(chain), hop)}, nil
	case *TagNode:
		switch n.Value.(type) {
		case *SequenceNode, *AliasNode, *AnchorNode:
			return e.resolveMergeValue(n.Value, mergeKey, chain)
		default:
			return e.resolveMergeValueWithSequence(n.Value, mergeKey, sequenceIndex, sequenceValue, chain)
		}
	case MapNode:
		return []MapNode{n}, [][]MergeHop{cloneChain(chain)}, nil
	default:
		return nil, nil, fmt.Errorf("%w: %T", ErrInvalidMergeValue, node)
	}
}

func mergeMapping(node interface{}) (*MappingNode, error) {
	switch n := node.(type) {
	case *File:
		if n == nil || len(n.Docs) == 0 || n.Docs[0] == nil || n.Docs[0].Body == nil {
			return nil, fmt.Errorf("%w: empty file", ErrInvalidMergeValue)
		}
		return mergeMapping(n.Docs[0].Body)
	case *MappingNode:
		return n, nil
	case *DocumentNode:
		if n == nil || n.Body == nil {
			return nil, fmt.Errorf("%w: empty document", ErrInvalidMergeValue)
		}
		return mergeMapping(n.Body)
	case *AnchorNode:
		if n == nil || n.Value == nil {
			return nil, fmt.Errorf("%w: empty anchor", ErrInvalidMergeValue)
		}
		return mergeMapping(n.Value)
	case *TagNode:
		if n == nil || n.Value == nil {
			return nil, fmt.Errorf("%w: empty tag", ErrInvalidMergeValue)
		}
		return mergeMapping(n.Value)
	default:
		return nil, fmt.Errorf("%w: %T", ErrInvalidMergeValue, node)
	}
}

func mergeKeyString(node Node) (string, error) {
	switch n := node.(type) {
	case *TagNode:
		return mergeKeyString(n.Value)
	case ScalarNode:
		return fmt.Sprint(n.GetValue()), nil
	default:
		return "", fmt.Errorf("%w: unsupported key type %T", ErrInvalidMergeValue, node)
	}
}

func mergeScalarString(node Node) (string, error) {
	switch n := node.(type) {
	case *StringNode:
		return n.Value, nil
	case *TagNode:
		return mergeScalarString(n.Value)
	case ScalarNode:
		return fmt.Sprint(n.GetValue()), nil
	default:
		return "", fmt.Errorf("%w: %T", ErrInvalidMergeValue, node)
	}
}

func sourceLocation(node Node) SourceLocation {
	if node == nil || node.GetToken() == nil || node.GetToken().Position == nil {
		return SourceLocation{Node: node}
	}
	position := node.GetToken().Position
	return SourceLocation{
		Node:     node,
		Position: position,
		Line:     position.Line,
		Column:   position.Column,
		Offset:   position.Offset,
	}
}

func cloneChain(chain []MergeHop) []MergeHop {
	if len(chain) == 0 {
		return nil
	}
	cloned := make([]MergeHop, len(chain))
	copy(cloned, chain)
	return cloned
}
