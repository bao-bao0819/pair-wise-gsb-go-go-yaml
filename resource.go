package yaml

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/goccy/go-yaml/ast"
)

// ResourceLimit restricts how much work a [Decoder] is allowed to perform
// while decoding a single input.
//
// A short YAML document can produce a huge expanded value by chaining a few
// dozen aliases (exponential amplification through shared anchors). Checking
// these budgets before any sizeable Go container is allocated keeps such
// inputs cheap to reject.
//
// The zero value of every field means "unlimited", so a [ResourceLimit]
// without explicit fields preserves the default decoder behavior.
type ResourceLimit struct {
	// MaxSyntaxNodes bounds the number of AST nodes in the parsed document,
	// counting the syntax tree itself without expanding aliases.
	MaxSyntaxNodes uint64

	// MaxAliasDepth bounds the length of any alias reference chain, measured
	// as the number of aliases that must be resolved while expanding a single
	// reference. A direct alias has depth 1.
	MaxAliasDepth uint64

	// MaxLogicalNodes bounds the estimated number of logical nodes in the
	// fully expanded value. Every reference to a shared anchor contributes the
	// anchor's full expanded weight, so alias amplification is charged here.
	MaxLogicalNodes uint64
}

// resourceLimitExceeded is the sentinel error wrapped by [ResourceLimitError].
var resourceLimitExceeded = errors.New("resource limit exceeded")

// ResourceLimitError reports that a [ResourceLimit] budget was exhausted.
//
// The error is returned before the decoder allocates containers for the
// expanded value.
type ResourceLimitError struct {
	// Kind identifies the exhausted budget: "syntax nodes", "alias depth" or
	// "logical nodes".
	Kind string
	// Limit is the configured budget.
	Limit uint64
	// Count is the (saturated) value that exceeded the budget.
	Count uint64
	// AliasPath describes the alias reference chain that caused the overflow,
	// e.g. ["a", "b"] means alias "*b" was reached while expanding alias "*a".
	// It is nil for syntax node violations.
	AliasPath []string
	// Node is the syntax node at which the violation was detected. It may be
	// nil when the overflow occurred while aggregating whole documents.
	Node ast.Node
}

func (e *ResourceLimitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "yaml: %s exceeded: limit %d exceeded by count >= %d", e.Kind, e.Limit, e.Count)
	if len(e.AliasPath) > 0 {
		fmt.Fprintf(&b, " via alias path %s", formatAliasPath(e.AliasPath))
	}
	if e.Node != nil && e.Node.GetToken() != nil && e.Node.GetToken().Position != nil {
		pos := e.Node.GetToken().Position
		fmt.Fprintf(&b, " at line %d column %d", pos.Line, pos.Column)
	}
	return b.String()
}

func (e *ResourceLimitError) Unwrap() error { return resourceLimitExceeded }

// IsResourceLimitError reports whether err (or any error in its chain) is a
// [*ResourceLimitError].
func IsResourceLimitError(err error) bool {
	return errors.Is(err, resourceLimitExceeded)
}

// aliasCycle is the sentinel error wrapped by [AliasCycleError].
var aliasCycle = errors.New("alias cycle detected")

// AliasCycleError reports a cyclic alias reference.
//
// A cycle is reported separately from alias depth violations so that genuine
// recursion (e.g. an anchor whose value references itself) is never hidden as
// an ordinary depth overflow.
type AliasCycleError struct {
	// Path is the alias reference chain that closes the cycle. The last entry
	// references an anchor already present earlier in the chain.
	Path []string
	// Node is the alias node that closes the cycle. It may be nil.
	Node ast.Node
}

func (e *AliasCycleError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "yaml: alias cycle detected: %s", formatAliasPath(e.Path))
	if e.Node != nil && e.Node.GetToken() != nil && e.Node.GetToken().Position != nil {
		pos := e.Node.GetToken().Position
		fmt.Fprintf(&b, " at line %d column %d", pos.Line, pos.Column)
	}
	return b.String()
}

func (e *AliasCycleError) Unwrap() error { return aliasCycle }

// IsAliasCycleError reports whether err (or any error in its chain) is an
// [*AliasCycleError].
func IsAliasCycleError(err error) bool {
	return errors.Is(err, aliasCycle)
}

func formatAliasPath(path []string) string {
	refs := make([]string, len(path))
	for i, name := range path {
		refs[i] = "*" + name
	}
	return strings.Join(refs, " -> ")
}

// satAdd returns a+b saturated at math.MaxUint64. Overflow counts as a budget
// violation: a saturated sum never passes a finite budget check.
func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// nodeInfo caches the estimated expanded weight of an anchor target and the
// longest alias chain reachable from it. Shared anchors are weighted once and
// reused, while every reference still charges the cached weight on its own.
type nodeInfo struct {
	weight uint64
	depth  uint64
	chain  []string
}

// resourceChecker validates a [ResourceLimit] against an AST without building
// the expanded Go value.
type resourceChecker struct {
	limit   ResourceLimit
	anchors map[string]ast.Node
	info    map[ast.Node]*nodeInfo

	// active holds the alias reference chain currently being expanded; it is
	// used both for cycle detection and for reporting overflow paths.
	active    []string
	activeSet map[string]struct{}
}

func newResourceChecker(limit ResourceLimit) *resourceChecker {
	return &resourceChecker{
		limit:     limit,
		anchors:   map[string]ast.Node{},
		info:      map[ast.Node]*nodeInfo{},
		activeSet: map[string]struct{}{},
	}
}

// seedAnchors makes anchors discovered in reference files resolvable without
// charging their syntax nodes to the current input.
func (c *resourceChecker) seedAnchors(anchors map[string]ast.Node) {
	for name, node := range anchors {
		c.anchors[name] = node
	}
}

// checkRoots runs every budget check over the given document roots.
func (c *resourceChecker) checkRoots(roots []ast.Node) error {
	var syntaxCount uint64
	for _, root := range roots {
		if root == nil {
			continue
		}
		count, err := c.countSyntax(root, syntaxCount)
		if err != nil {
			return err
		}
		syntaxCount = count
	}
	// Phase 2: weight each root once to detect cycles and alias depth
	// violations independently of the logical budget, then charge the budget
	// by source order so overflow paths point at the responsible reference.
	for _, root := range roots {
		if root == nil {
			continue
		}
		if _, err := c.nodeInfoFor(root); err != nil {
			return err
		}
	}
	if c.limit.MaxLogicalNodes == 0 {
		// No logical budget: cycle and depth checks were already applied by
		// nodeInfoFor above, so there is nothing else to enforce.
		return nil
	}
	var total uint64
	for _, root := range roots {
		if root == nil {
			continue
		}
		next, err := c.chargeLogical(root, total)
		if err != nil {
			return err
		}
		total = next
	}
	return nil
}

// chargeLogical charges the estimated expanded tree against the logical node
// budget in source order. Shared anchor weights come from the cache, while
// every reference charges the weight on its own; the alias site whose
// contribution tips the budget is reported with its reference path.
//
// Traversal follows the physical tree only (alias references are not
// descended), and charging stops as soon as the budget is exceeded, so a
// highly amplified graph is rejected after touching only its small AST.
func (c *resourceChecker) chargeLogical(node ast.Node, total uint64) (uint64, error) {
	if node == nil {
		return total, nil
	}
	if alias, ok := node.(*ast.AliasNode); ok {
		return c.chargeAlias(alias, total)
	}
	if anchor, ok := node.(*ast.AnchorNode); ok {
		if anchor.Name != nil {
			if name, ok := anchorName(anchor); ok {
				c.anchors[name] = anchor.Value
			}
		}
		return c.chargeLogical(anchor.Value, total)
	}
	if doc, ok := node.(*ast.DocumentNode); ok {
		return c.chargeLogical(doc.Body, total)
	}
	if entry, ok := node.(*ast.MappingValueNode); ok {
		return c.chargeLogical(entry.Value, total)
	}
	if entry, ok := node.(*ast.MappingKeyNode); ok {
		return c.chargeLogical(entry.Value, total)
	}

	// Children are accounted before the container itself, so an alias child
	// that tips the budget is attributed to the alias rather than the parent.
	for _, child := range logicalChildren(node) {
		if alias, ok := child.(*ast.AliasNode); ok {
			next, err := c.chargeAlias(alias, total)
			if err != nil {
				return total, err
			}
			total = next
			continue
		}
		if anchor, ok := child.(*ast.AnchorNode); ok {
			next, err := c.chargeLogical(anchor.Value, total)
			if err != nil {
				return next, err
			}
			total = next
			continue
		}
		// Non-alias child: recurse so nested containers are charged with the
		// same ordering guarantees.
		next, err := c.chargeLogical(child, total)
		if err != nil {
			return next, err
		}
		total = next
	}
	total = satAdd(total, 1)
	if err := c.logicalExceeded(total, nil, node); err != nil {
		return total, err
	}
	return total, nil
}

// logicalExceeded returns a [*ResourceLimitError] when count violates the
// logical node budget, or nil otherwise.
func (c *resourceChecker) logicalExceeded(count uint64, path []string, node ast.Node) error {
	if c.limit.MaxLogicalNodes == 0 || count <= c.limit.MaxLogicalNodes {
		return nil
	}
	return &ResourceLimitError{
		Kind:      "logical nodes",
		Limit:     c.limit.MaxLogicalNodes,
		Count:     count,
		AliasPath: path,
		Node:      node,
	}
}

// chargeAlias charges one alias reference against the logical budget using
// the cached expansion weight of its target. The weight is validated for
// cycles and depth by nodeInfoFor; this never descends into the target.
func (c *resourceChecker) chargeAlias(alias *ast.AliasNode, total uint64) (uint64, error) {
	name, hasName := aliasName(alias)
	if !hasName {
		next := satAdd(total, 1)
		return next, c.logicalExceeded(next, nil, alias)
	}
	info, err := c.nodeInfoFor(alias)
	if err != nil {
		return total, err
	}
	next := satAdd(total, info.weight)
	path := info.chain
	if len(path) == 0 {
		path = []string{name}
	}
	if err := c.logicalExceeded(next, path, alias); err != nil {
		return next, err
	}
	return next, nil
}

// countSyntax walks only the AST reference graph (aliases are treated as leaf
// references, never expanded) and applies the syntax node budget before any
// alias-aware work happens.
func (c *resourceChecker) countSyntax(node ast.Node, count uint64) (uint64, error) {
	var walk func(ast.Node) error
	walk = func(n ast.Node) error {
		if n == nil {
			return nil
		}
		count = satAdd(count, 1)
		if c.limit.MaxSyntaxNodes != 0 && count > c.limit.MaxSyntaxNodes {
			return &ResourceLimitError{
				Kind:  "syntax nodes",
				Limit: c.limit.MaxSyntaxNodes,
				Count: count,
				Node:  n,
			}
		}
		if anchor, ok := n.(*ast.AnchorNode); ok {
			if anchor.Name != nil {
				if err := walk(anchor.Name); err != nil {
					return err
				}
			}
			if anchor.Value != nil {
				if name, ok := anchorName(anchor); ok {
					c.anchors[name] = anchor.Value
				}
				if err := walk(anchor.Value); err != nil {
					return err
				}
			}
			return nil
		}
		// Alias references are not followed: only the reference name in the
		// AST graph is counted, the target is never expanded here.
		if alias, ok := n.(*ast.AliasNode); ok {
			if alias.Value != nil {
				return walk(alias.Value)
			}
			return nil
		}
		for _, child := range syntaxChildren(n) {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(node); err != nil {
		return count, err
	}
	return count, nil
}

// nodeInfoFor returns the cached expanded weight of node, computing it on the
// first encounter. Cycles and alias depth violations are reported here.
func (c *resourceChecker) nodeInfoFor(node ast.Node) (*nodeInfo, error) {
	if node == nil {
		return &nodeInfo{}, nil
	}
	if cached, ok := c.info[node]; ok {
		return cached, nil
	}
	if n, ok := node.(*ast.AliasNode); ok {
		name, hasName := aliasName(n)
		if !hasName {
			return &nodeInfo{weight: 1}, nil
		}
		if _, cycling := c.activeSet[name]; cycling {
			return nil, &AliasCycleError{Path: c.appendActive(name), Node: n}
		}
		target, exists := c.anchors[name]
		if !exists {
			// Undefined aliases are reported by the decoder itself; the
			// resource check must not produce a false positive for them.
			return &nodeInfo{weight: 1}, nil
		}
		c.pushAlias(name)
		info, err := c.nodeInfoFor(target)
		c.popAlias()
		if err != nil {
			return nil, err
		}
		depth := satAdd(info.depth, 1)
		if c.limit.MaxAliasDepth != 0 && depth > c.limit.MaxAliasDepth {
			chain := c.extendActive(name, info.chain)
			return nil, &ResourceLimitError{
				Kind:      "alias depth",
				Limit:     c.limit.MaxAliasDepth,
				Count:     depth,
				AliasPath: chain,
				Node:      n,
			}
		}
		res := &nodeInfo{weight: info.weight, depth: depth, chain: c.extendActive(name, info.chain)}
		c.info[node] = res
		return res, nil
	}
	if n, ok := node.(*ast.AnchorNode); ok {
		if n.Name != nil {
			if name, ok := anchorName(n); ok {
				c.anchors[name] = n.Value
			}
		}
		return c.nodeInfoFor(n.Value)
	}
	if n, ok := node.(*ast.DocumentNode); ok {
		return c.nodeInfoFor(n.Body)
	}
	if n, ok := node.(*ast.MappingValueNode); ok {
		return c.nodeInfoFor(n.Value)
	}
	if n, ok := node.(*ast.MappingKeyNode); ok {
		return c.nodeInfoFor(n.Value)
	}

	weight := uint64(1)
	var depth uint64
	var chain []string
	for _, child := range logicalChildren(node) {
		info, err := c.nodeInfoFor(child)
		if err != nil {
			return nil, err
		}
		weight = satAdd(weight, info.weight)
		if info.depth > depth {
			depth = info.depth
			chain = info.chain
		}
	}
	res := &nodeInfo{weight: weight, depth: depth, chain: chain}
	c.info[node] = res
	return res, nil
}

func (c *resourceChecker) pushAlias(name string) {
	c.active = append(c.active, name)
	c.activeSet[name] = struct{}{}
}

func (c *resourceChecker) popAlias() {
	last := c.active[len(c.active)-1]
	c.active = c.active[:len(c.active)-1]
	delete(c.activeSet, last)
}

// appendActive returns a copy of the current chain with closing appended.
func (c *resourceChecker) appendActive(closing string) []string {
	path := make([]string, 0, len(c.active)+1)
	path = append(path, c.active...)
	path = append(path, closing)
	return path
}

// extendActive reports the full reference path from the current expansion
// site through name and, when available, the longest chain inside the named
// anchor target.
func (c *resourceChecker) extendActive(name string, internal []string) []string {
	path := make([]string, 0, len(c.active)+1+len(internal))
	path = append(path, c.active...)
	path = append(path, name)
	path = append(path, internal...)
	return path
}

// syntaxChildren enumerates every child node that is part of the AST syntax
// tree, mirroring [ast.Walk]. Alias and anchor wrappers are handled by callers.
func syntaxChildren(n ast.Node) []ast.Node {
	switch v := n.(type) {
	case *ast.DocumentNode:
		return []ast.Node{v.Body}
	case *ast.MappingNode:
		children := make([]ast.Node, 0, len(v.Values))
		for _, value := range v.Values {
			children = append(children, value)
		}
		return children
	case *ast.MappingValueNode:
		return []ast.Node{v.Key, v.Value}
	case *ast.MappingKeyNode:
		return []ast.Node{v.Value}
	case *ast.SequenceNode:
		children := make([]ast.Node, 0, len(v.Values))
		for _, value := range v.Values {
			children = append(children, value)
		}
		return children
	case *ast.DirectiveNode:
		children := make([]ast.Node, 0, 1+len(v.Values))
		children = append(children, v.Name)
		children = append(children, v.Values...)
		return children
	case *ast.SequenceEntryNode:
		return []ast.Node{v.Value}
	case *ast.TagNode:
		return []ast.Node{v.Value}
	case *ast.LiteralNode:
		return []ast.Node{v.Value}
	default:
		return nil
	}
}

// logicalChildren enumerates the child nodes that contribute to the expanded
// value. A mapping entry contributes only its value: scalar keys are folded
// into the parent map and add no logical node. Alias, anchor and document
// wrappers are handled by callers.
func logicalChildren(n ast.Node) []ast.Node {
	switch v := n.(type) {
	case *ast.MappingNode:
		children := make([]ast.Node, 0, len(v.Values))
		for _, value := range v.Values {
			if value != nil {
				children = append(children, value.Value)
			}
		}
		return children
	case *ast.SequenceNode:
		children := make([]ast.Node, 0, len(v.Values))
		for _, value := range v.Values {
			children = append(children, value)
		}
		return children
	case *ast.SequenceEntryNode:
		return []ast.Node{v.Value}
	case *ast.TagNode:
		return []ast.Node{v.Value}
	case *ast.LiteralNode:
		return []ast.Node{v.Value}
	default:
		return nil
	}
}

func anchorName(n *ast.AnchorNode) (string, bool) {
	if n == nil || n.Name == nil {
		return "", false
	}
	return nodeName(n.Name)
}

func aliasName(n *ast.AliasNode) (string, bool) {
	if n == nil || n.Value == nil {
		return "", false
	}
	return nodeName(n.Value)
}

func nodeName(n ast.Node) (string, bool) {
	if s, ok := n.(*ast.StringNode); ok {
		return s.Value, true
	}
	tk := n.GetToken()
	if tk == nil {
		return "", false
	}
	return tk.Value, true
}

// CheckResourceLimit validates limit against a parsed AST node without
// decoding it. The traversal follows only the AST reference graph; aliases
// are weighted for expansion semantics but never materialized, and no Go
// containers for the expanded value are allocated.
//
// Multiple nodes may be passed to validate a multi-document stream against a
// single shared budget (e.g. f.Docs[0], f.Docs[1], ...).
func CheckResourceLimit(limit ResourceLimit, nodes ...ast.Node) error {
	c := newResourceChecker(limit)
	return c.checkRoots(nodes)
}

// CheckResourceLimit validates the decoder's configured limit against a
// parsed AST node without decoding it.
//
// Multiple nodes may be passed to validate a multi-document stream against a
// single shared budget.
func (d *Decoder) CheckResourceLimit(nodes ...ast.Node) error {
	if d.resourceLimit == nil {
		return nil
	}
	c := newResourceChecker(*d.resourceLimit)
	c.seedAnchors(d.anchorNodeMap)
	return c.checkRoots(nodes)
}

// checkParsedFile validates the decoder's configured limit against a freshly
// parsed file, before decoding allocates the expanded value.
func (d *Decoder) checkParsedFile(f *ast.File) error {
	if d.resourceLimit == nil {
		return nil
	}
	c := newResourceChecker(*d.resourceLimit)
	c.seedAnchors(d.anchorNodeMap)
	return c.checkRoots(documentRoots(f))
}

func documentRoots(f *ast.File) []ast.Node {
	roots := make([]ast.Node, 0, len(f.Docs))
	for _, doc := range f.Docs {
		if doc != nil {
			roots = append(roots, doc)
		}
	}
	return roots
}
