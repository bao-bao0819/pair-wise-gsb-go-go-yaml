package yaml

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/internal/errors"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// ResourceLimitKind identifies which decoder resource limit was exceeded.
type ResourceLimitKind int

const (
	// SyntaxNodeLimit limits the number of syntax nodes present in the parsed
	// source documents, independently of any alias expansion.
	SyntaxNodeLimit ResourceLimitKind = iota + 1
	// AliasDepthLimit limits the number of alias references that may be nested
	// along a single expansion path.
	AliasDepthLimit
	// LogicalNodeLimit limits the estimated number of nodes that would result
	// from expanding every alias reference in the documents.
	LogicalNodeLimit
)

func (k ResourceLimitKind) String() string {
	switch k {
	case SyntaxNodeLimit:
		return "syntax node"
	case AliasDepthLimit:
		return "alias reference depth"
	case LogicalNodeLimit:
		return "logical node"
	default:
		return "unknown resource"
	}
}

// ResourceLimits configures the decoder-level resource policy.
//
// Every limit is isolated to the decoder instance it is configured on and the
// zero value disables all limits, preserving the default behavior. A negative
// value is invalid and reported when the option is applied.
//
// MaxSyntaxNodes limits the number of syntax nodes contained in the parsed
// documents. It is computed without following alias references.
//
// MaxAliasDepth limits how many alias references may be nested along a single
// expansion path. Cyclic alias references are never reported as depth
// violations; they produce an [AliasCycleError] instead.
//
// MaxLogicalNodes limits the estimated number of nodes produced when every
// alias is expanded into its target. The weight of a shared anchor target is
// computed once and cached, but every reference still contributes that weight
// to the total, so amplification through repeated references is accounted for.
type ResourceLimits struct {
	MaxSyntaxNodes  int64
	MaxAliasDepth   int64
	MaxLogicalNodes int64
}

func (l ResourceLimits) enabled() bool {
	return l.MaxSyntaxNodes > 0 || l.MaxAliasDepth > 0 || l.MaxLogicalNodes > 0
}

func (l ResourceLimits) validate() error {
	if l.MaxSyntaxNodes < 0 {
		return fmt.Errorf("MaxSyntaxNodes must not be negative: " + strconv.FormatInt(l.MaxSyntaxNodes, 10))
	}
	if l.MaxAliasDepth < 0 {
		return fmt.Errorf("MaxAliasDepth must not be negative: " + strconv.FormatInt(l.MaxAliasDepth, 10))
	}
	if l.MaxLogicalNodes < 0 {
		return fmt.Errorf("MaxLogicalNodes must not be negative: " + strconv.FormatInt(l.MaxLogicalNodes, 10))
	}
	return nil
}

// WithResourceLimits sets the decoder-level resource policy.
func WithResourceLimits(limits ResourceLimits) DecodeOption {
	return func(d *Decoder) error {
		d.resourceLimits = limits
		d.resourceLimitsSet = true
		return limits.validate()
	}
}

// ResourceLimitError reports that a decoder resource limit was exceeded before
// the oversized result was allocated.
type ResourceLimitError struct {
	// Kind identifies the violated limit.
	Kind ResourceLimitKind
	// Limit is the configured limit value.
	Limit int64
	// Count is the estimated or observed value that crossed the limit. It is
	// saturated at the maximum int64 value on arithmetic overflow.
	Count int64
	// Path is the sequence of anchor names reached through alias references
	// that led to the violation, starting from the document body. It is empty
	// for syntax node violations.
	Path []string
	// Token marks the source location that triggered the violation.
	Token *token.Token
}

func (e *ResourceLimitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s limit exceeded: ", e.Kind)
	switch e.Kind {
	case AliasDepthLimit:
		fmt.Fprintf(&b, "depth %d exceeds limit %d", e.Count, e.Limit)
	default:
		fmt.Fprintf(&b, "%d nodes exceeds limit %d", e.Count, e.Limit)
	}
	if len(e.Path) > 0 {
		fmt.Fprintf(&b, " via alias reference path: %s", formatAliasPath(e.Path))
	}
	return errors.FormatError(b.String(), e.Token, false, true)
}

// GetToken returns the source token associated with the violation.
func (e *ResourceLimitError) GetToken() *token.Token {
	return e.Token
}

// AliasCycleError reports a cyclic chain of alias references. Cycles are
// reported separately from depth violations so that a finite, but too deep,
// expansion is not confused with an unbounded one.
type AliasCycleError struct {
	// Path is the alias reference chain that closes the cycle. The repeated
	// anchor name appears both in the middle and at the end of the path.
	Path []string
	// Token marks the alias reference that closes the cycle.
	Token *token.Token
}

func (e *AliasCycleError) Error() string {
	msg := "cyclic alias reference detected: " + formatAliasPath(e.Path)
	return errors.FormatError(msg, e.Token, false, true)
}

// GetToken returns the source token associated with the cycle.
func (e *AliasCycleError) GetToken() *token.Token {
	return e.Token
}

// IsResourceLimitError reports whether err is a [ResourceLimitError] and, when
// kind is non-zero, whether it reports the given limit kind.
func IsResourceLimitError(err error, kind ResourceLimitKind) bool {
	var e *ResourceLimitError
	if !errors.As(err, &e) {
		return false
	}
	return kind == 0 || e.Kind == kind
}

// IsAliasCycleError reports whether err is an [AliasCycleError].
func IsAliasCycleError(err error) bool {
	var e *AliasCycleError
	return errors.As(err, &e)
}

func formatAliasPath(path []string) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	for i, name := range path {
		if i > 0 {
			b.WriteString(" -> ")
		}
		b.WriteString("*")
		b.WriteString(name)
	}
	return b.String()
}

const saturatedMax = int64(^uint64(0) >> 1)

// satAdd returns a+b saturated at the platform int64 maximum.
func satAdd(a, b int64) int64 {
	if b > saturatedMax-a {
		return saturatedMax
	}
	return a + b
}

type resourceChecker struct {
	limits ResourceLimits
	// anchors maps every anchor name found in the checked file to the node
	// bound by the most recent definition of that name.
	anchors map[string]ast.Node
	// edges maps an anchor target node to the alias references reachable from
	// it without crossing another anchor definition.
	edges map[ast.Node][]aliasEdge
	// depthCache memoizes the longest alias chain leaving a node.
	depthCache map[ast.Node]int64
	// weightState/weightCache memoize the expanded logical weight of a node.
	weightState map[ast.Node]int
	weightCache map[ast.Node]int64
	// depthSuffix/depthAlias memoize the deepest alias chain leaving an
	// anchor target, relative to its entry point, first hop first.
	depthSuffix map[ast.Node][]string
	depthAlias  map[ast.Node][]*ast.AliasNode
}

type aliasEdge struct {
	name  string
	alias *ast.AliasNode
}

const (
	nodeVisiting = 1
	nodeVisited  = 2
)

func newResourceChecker(limits ResourceLimits) *resourceChecker {
	return &resourceChecker{
		limits:      limits,
		anchors:     map[string]ast.Node{},
		edges:       map[ast.Node][]aliasEdge{},
		depthCache:  map[ast.Node]int64{},
		depthSuffix: map[ast.Node][]string{},
		depthAlias:  map[ast.Node][]*ast.AliasNode{},
		weightState: map[ast.Node]int{},
		weightCache: map[ast.Node]int64{},
	}
}

func docToken(doc *ast.DocumentNode) *token.Token {
	if doc.Body != nil {
		return doc.Body.GetToken()
	}
	return doc.Start
}

func (c *resourceChecker) depthError(depth int64, p chain) error {
	return &ResourceLimitError{
		Kind:  AliasDepthLimit,
		Limit: c.limits.MaxAliasDepth,
		Count: depth,
		Path:  p.names,
		Token: p.limitToken(c.limits.MaxAliasDepth),
	}
}

// limitToken returns the token of the alias reference at the first hop that
// crosses the configured depth limit, falling back to the deepest hop.
func (p chain) limitToken(limit int64) *token.Token {
	if len(p.aliases) == 0 {
		return nil
	}
	idx := int(limit)
	if idx > len(p.aliases) {
		idx = len(p.aliases) - 1
	}
	return p.aliases[idx].GetToken()
}

// check enforces the resource policy on the parsed file.
//
// When expand is false only the AST reference graph is traversed: syntax
// nodes are counted, cycles are reported and the static longest alias chain is
// measured. Logical node estimation (which traverses every alias expansion) is
// skipped, and alias names that cannot be resolved locally are ignored
// instead of followed.
//
// When expand is true the expanded logical weight is estimated as well.
// An alias that cannot be resolved locally counts as a single node; the
// decoder reports it as a syntax error during normalization.
func (c *resourceChecker) check(file *ast.File, expand bool) error {
	for _, doc := range file.Docs {
		c.collectAnchors(doc.Body)
	}
	if err := c.checkSyntax(file); err != nil {
		return err
	}
	// Cycle detection and chain measurement walk the reference graph itself
	// and never clone or expand alias targets.
	if err := c.checkCycles(file); err != nil {
		return err
	}
	if c.limits.MaxAliasDepth > 0 {
		for _, doc := range file.Docs {
			depth, p := c.longestChain(doc.Body)
			if depth > c.limits.MaxAliasDepth {
				return c.depthError(depth, p)
			}
		}
	}
	if expand && c.limits.MaxLogicalNodes > 0 {
		for _, doc := range file.Docs {
			weight, p, _ := c.logicalWeight(doc.Body, chain{}, c.limits.MaxLogicalNodes)
			if weight > c.limits.MaxLogicalNodes {
				tk := c.tokenForChain(p)
				if tk == nil {
					tk = docToken(doc)
				}
				return &ResourceLimitError{
					Kind:  LogicalNodeLimit,
					Limit: c.limits.MaxLogicalNodes,
					Count: weight,
					Path:  p.names,
					Token: tk,
				}
			}
		}
	}
	return nil
}

// collectAnchors records every anchor definition reachable from node. Later
// definitions of the same name override earlier ones, matching decoder
// resolution order.
func (c *resourceChecker) collectAnchors(node ast.Node) {
	ast.Walk(anchorCollector{checker: c}, node)
}

type anchorCollector struct {
	checker *resourceChecker
}

func (v anchorCollector) Visit(node ast.Node) ast.Visitor {
	if anchor, ok := node.(*ast.AnchorNode); ok {
		if anchor.Name != nil {
			v.checker.anchors[anchor.Name.GetToken().Value] = anchor.Value
		}
	}
	return v
}

// checkSyntax counts syntax nodes in each document body without following
// alias references. The anchor node, alias node and the alias name scalar are
// syntax nodes; the target referenced by an alias is not traversed here.
func (c *resourceChecker) checkSyntax(file *ast.File) error {
	if c.limits.MaxSyntaxNodes <= 0 {
		return nil
	}
	for _, doc := range file.Docs {
		count, tk := countSyntaxNodes(doc.Body)
		if count > c.limits.MaxSyntaxNodes {
			return &ResourceLimitError{
				Kind:  SyntaxNodeLimit,
				Limit: c.limits.MaxSyntaxNodes,
				Count: count,
				Token: tk,
			}
		}
	}
	return nil
}

// countSyntaxNodes counts the syntax nodes reachable from node without
// following alias references. The count is saturated at saturatedMax.
func countSyntaxNodes(node ast.Node) (int64, *token.Token) {
	var count int64
	var overflowToken *token.Token
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		if count < saturatedMax {
			count++
			if overflowToken == nil && n.GetToken() != nil {
				overflowToken = n.GetToken()
			}
		}
		switch t := n.(type) {
		case *ast.DocumentNode:
			walk(t.Body)
		case *ast.LiteralNode:
			walk(t.Value)
		case *ast.DirectiveNode:
			walk(t.Name)
			for _, value := range t.Values {
				walk(value)
			}
		case *ast.TagNode:
			walk(t.Value)
		case *ast.MappingNode:
			for _, value := range t.Values {
				walk(value)
			}
		case *ast.MappingKeyNode:
			walk(t.Value)
		case *ast.MappingValueNode:
			walk(t.Key)
			walk(t.Value)
		case *ast.SequenceNode:
			for _, value := range t.Values {
				walk(value)
			}
		case *ast.AnchorNode:
			walk(t.Name)
			walk(t.Value)
		case *ast.AliasNode:
			// The alias name scalar is a syntax node, but the referenced
			// anchor target belongs to another location in the document.
			walk(t.Value)
		}
	}
	walk(node)
	return count, overflowToken
}

// referenceEdges returns the alias references directly reachable from an
// anchor target without crossing another anchor definition. Results are
// memoized on the anchor target node.
func (c *resourceChecker) referenceEdges(node ast.Node) []aliasEdge {
	if node == nil {
		return nil
	}
	if edges, ok := c.edges[node]; ok {
		return edges
	}
	var edges []aliasEdge
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		switch t := n.(type) {
		case nil:
			return
		case *ast.AliasNode:
			edges = append(edges, aliasEdge{
				name:  t.Value.GetToken().Value,
				alias: t,
			})
			return
		case *ast.AnchorNode:
			// A nested anchor defines its own reference scope.
			return
		case *ast.DocumentNode:
			walk(t.Body)
		case *ast.LiteralNode:
			walk(t.Value)
		case *ast.DirectiveNode:
			walk(t.Name)
			for _, value := range t.Values {
				walk(value)
			}
		case *ast.TagNode:
			walk(t.Value)
		case *ast.MappingNode:
			for _, value := range t.Values {
				walk(value)
			}
		case *ast.MappingKeyNode:
			walk(t.Value)
		case *ast.MappingValueNode:
			walk(t.Key)
			walk(t.Value)
		case *ast.SequenceNode:
			for _, value := range t.Values {
				walk(value)
			}
		}
	}
	walk(node)
	c.edges[node] = edges
	return edges
}

// checkCycles detects cyclic alias references by walking the anchor reference
// graph. The walk only follows name-to-target edges; it never expands targets.
// Cycles are reported independently of depth violations.
func (c *resourceChecker) checkCycles(file *ast.File) error {
	state := map[string]int{}
	var stack []string
	var visit func(name string, via *ast.AliasNode) error
	visit = func(name string, via *ast.AliasNode) error {
		switch state[name] {
		case nodeVisited:
			return nil
		case nodeVisiting:
			for i, n := range stack {
				if n == name {
					path := append([]string{}, stack[i:]...)
					path = append(path, name)
					tk := cycleToken(via, c.anchors[name])
					if closing := c.cycleClosingAlias(path); closing != nil {
						tk = closing.GetToken()
					}
					return &AliasCycleError{Path: path, Token: tk}
				}
			}
		}
		state[name] = nodeVisiting
		stack = append(stack, name)
		target, resolved := c.anchors[name]
		if resolved {
			for _, edge := range c.referenceEdges(target) {
				if _, ok := c.anchors[edge.name]; ok {
					if err := visit(edge.name, edge.alias); err != nil {
						return err
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = nodeVisited
		return nil
	}
	names := make([]string, 0, len(c.anchors))
	for name := range c.anchors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return err
		}
	}
	return nil
}

func cycleToken(via *ast.AliasNode, target ast.Node) *token.Token {
	if via != nil {
		return via.GetToken()
	}
	if target != nil {
		return target.GetToken()
	}
	return nil
}

// cycleClosingAlias finds the alias reference that closes a cycle: the alias
// to the final path name reachable from the preceding anchor in the path.
func (c *resourceChecker) cycleClosingAlias(path []string) *ast.AliasNode {
	if len(path) < 2 {
		return nil
	}
	from := c.anchors[path[len(path)-2]]
	target := path[len(path)-1]
	for _, edge := range c.referenceEdges(from) {
		if edge.name == target {
			return edge.alias
		}
	}
	return nil
}

// chain is an alias reference path in traversal order: names[i] is the anchor
// entered at hop i and aliases[i] is the alias node performing that hop.
type chain struct {
	names   []string
	aliases []*ast.AliasNode
	site    ast.Node
}

func (p chain) appendTo(alias *ast.AliasNode) chain {
	names := make([]string, 0, len(p.names)+1)
	names = append(names, p.names...)
	names = append(names, alias.Value.GetToken().Value)
	aliases := make([]*ast.AliasNode, 0, len(p.aliases)+1)
	aliases = append(aliases, p.aliases...)
	aliases = append(aliases, alias)
	return chain{names: names, aliases: aliases}
}

func (p chain) concat(suffix chain) chain {
	names := make([]string, 0, len(p.names)+len(suffix.names))
	names = append(names, p.names...)
	names = append(names, suffix.names...)
	aliases := make([]*ast.AliasNode, 0, len(p.aliases)+len(suffix.aliases))
	aliases = append(aliases, p.aliases...)
	aliases = append(aliases, suffix.aliases...)
	return chain{names: names, aliases: aliases}
}

func (p chain) token() *token.Token {
	if len(p.aliases) == 0 {
		return nil
	}
	return p.aliases[len(p.aliases)-1].GetToken()
}

func (c *resourceChecker) tokenForChain(p chain) *token.Token {
	if tk := p.token(); tk != nil {
		return tk
	}
	if p.site != nil {
		return p.site.GetToken()
	}
	if len(p.names) > 0 {
		if node, ok := c.anchors[p.names[len(p.names)-1]]; ok {
			return node.GetToken()
		}
	}
	return nil
}

// longestChain returns the number of alias references in the longest nested
// expansion path leaving node, together with the chain in traversal order.
//
// The suffix for an anchor target is memoized: a shared target is measured
// once, but every caller still adds the reference leading to it. Cycles are
// assumed to have been rejected by [resourceChecker.checkCycles].
func (c *resourceChecker) longestChain(node ast.Node) (int64, chain) {
	return c.chainAt(node, chain{}, map[ast.Node]struct{}{})
}

func (c *resourceChecker) chainAt(
	node ast.Node, prefix chain, active map[ast.Node]struct{},
) (int64, chain) {
	switch t := node.(type) {
	case nil:
		return 0, prefix
	case *ast.AliasNode:
		target, resolved := c.anchors[t.Value.GetToken().Value]
		if !resolved {
			return 0, prefix
		}
		depth, cp := c.chainTarget(target, prefix.appendTo(t), active)
		return depth + 1, cp
	case *ast.AnchorNode:
		return c.chainAt(t.Value, prefix, active)
	case *ast.TagNode:
		return c.chainAt(t.Value, prefix, active)
	case *ast.LiteralNode:
		return c.chainAt(t.Value, prefix, active)
	case *ast.MappingNode:
		return c.maxChild(childrenOfMapping(t), prefix, active)
	case *ast.MappingValueNode:
		return c.maxChild([]ast.Node{t.Key, t.Value}, prefix, active)
	case *ast.MappingKeyNode:
		return c.chainAt(t.Value, prefix, active)
	case *ast.SequenceNode:
		children := make([]ast.Node, 0, len(t.Values))
		for _, value := range t.Values {
			children = append(children, value)
		}
		return c.maxChild(children, prefix, active)
	case *ast.DocumentNode:
		return c.chainAt(t.Body, prefix, active)
	case *ast.DirectiveNode:
		return c.maxChild(append([]ast.Node{t.Name}, t.Values...), prefix, active)
	default:
		return 0, prefix
	}
}

func (c *resourceChecker) chainTarget(
	target ast.Node, prefix chain, active map[ast.Node]struct{},
) (int64, chain) {
	if depth, ok := c.depthCache[target]; ok {
		return depth, prefix.concat(chain{
			names:   append([]string{}, c.depthSuffix[target]...),
			aliases: append([]*ast.AliasNode{}, c.depthAlias[target]...),
		})
	}
	if _, visiting := active[target]; visiting {
		// Defensive: checkCycles reports this explicitly.
		return 0, prefix
	}
	active[target] = struct{}{}
	depth, suffix, path := c.chainFromTarget(target, prefix, active)
	delete(active, target)
	c.depthCache[target] = depth
	if len(suffix.names) > 0 {
		c.depthSuffix[target] = append([]string{}, suffix.names...)
		c.depthAlias[target] = append(
			[]*ast.AliasNode{}, suffix.aliases...,
		)
	}
	return depth, path
}

// chainFromTarget measures the alias chain internal to target, relative to
// the chain that already entered it. It returns the target-relative suffix,
// the full chain and the depth counted from inside the target.
func (c *resourceChecker) chainFromTarget(
	target ast.Node, prefix chain, active map[ast.Node]struct{},
) (int64, chain, chain) {
	before := len(prefix.names)
	depth, path := c.chainAt(target, prefix, active)
	suffix := chain{
		names:   append([]string{}, path.names[before:]...),
		aliases: append([]*ast.AliasNode{}, path.aliases[before:]...),
	}
	return depth, suffix, path
}

func (c *resourceChecker) maxChild(
	children []ast.Node, prefix chain, active map[ast.Node]struct{},
) (int64, chain) {
	var maxDepth int64 = -1
	var maxPath chain
	for _, child := range children {
		depth, path := c.chainAt(child, prefix, active)
		if depth > maxDepth {
			maxDepth = depth
			maxPath = path
		}
	}
	if maxDepth < 0 {
		return 0, prefix
	}
	return maxDepth, maxPath
}

// logicalWeight estimates the number of logical nodes produced when every
// alias reachable from node is expanded. It returns the saturated weight and,
// when a subtree exceeds budget, the alias reference chain into that subtree.
//
// The weight of a shared anchor target is cached. Every alias still recurses
// into the target and adds its full weight to the containing container, so
// the contribution of repeated references is always accounted for.
func (c *resourceChecker) logicalWeight(
	node ast.Node, prefix chain, budget int64,
) (int64, chain, bool) {
	switch t := node.(type) {
	case nil:
		return 0, prefix, false
	case *ast.AliasNode:
		target, resolved := c.anchors[t.Value.GetToken().Value]
		if !resolved {
			// Unresolved aliases count as a single node; decoding reports
			// them as syntax errors.
			return 1, prefix, false
		}
		weight, path, exceeded := c.weightTarget(target, prefix.appendTo(t), budget)
		if path.site == nil {
			path.site = t
		}
		return weight, path, exceeded
	case *ast.AnchorNode:
		path := prefix
		path.site = t
		w, p, ex := c.logicalWeight(t.Value, path, budget)
		return w, p, ex
	case *ast.TagNode:
		return c.logicalWeight(t.Value, prefix, budget)
	case *ast.LiteralNode:
		return c.logicalWeight(t.Value, prefix, budget)
	case *ast.MappingNode:
		return c.weightChildren(childrenOfMapping(t), prefix, budget)
	case *ast.MappingValueNode:
		return c.weightChildren([]ast.Node{t.Key, t.Value}, prefix, budget)
	case *ast.MappingKeyNode:
		return c.logicalWeight(t.Value, prefix, budget)
	case *ast.SequenceNode:
		children := make([]ast.Node, 0, len(t.Values))
		for _, value := range t.Values {
			children = append(children, value)
		}
		return c.weightChildren(children, prefix, budget)
	case *ast.DocumentNode:
		return c.logicalWeight(t.Body, prefix, budget)
	case *ast.DirectiveNode:
		return c.weightChildren(append([]ast.Node{t.Name}, t.Values...), prefix, budget)
	default:
		return 1, prefix, false
	}
}

func (c *resourceChecker) weightTarget(
	target ast.Node, prefix chain, budget int64,
) (int64, chain, bool) {
	if weight, ok := c.weightCache[target]; ok {
		return weight, prefix, weight > budget
	}
	if c.weightState[target] == nodeVisiting {
		// Defensive: cycles are rejected before weight estimation.
		return 0, prefix, false
	}
	c.weightState[target] = nodeVisiting
	weight, path, exceeded := c.logicalWeight(target, prefix, budget)
	c.weightState[target] = nodeVisited
	if !exceeded {
		c.weightCache[target] = weight
	}
	return weight, path, exceeded
}

func (c *resourceChecker) weightChildren(
	children []ast.Node, prefix chain, budget int64,
) (int64, chain, bool) {
	total := int64(1)
	var last chain
	for _, child := range children {
		weight, path, exceeded := c.logicalWeight(child, prefix, budget)
		if path.site == nil {
			path.site = child
		}
		last = path
		if exceeded {
			return satAdd(total, weight), path, true
		}
		total = satAdd(total, weight)
		if total > budget {
			return total, last, true
		}
	}
	return total, prefix, false
}

func childrenOfMapping(node *ast.MappingNode) []ast.Node {
	children := make([]ast.Node, 0, len(node.Values))
	for _, value := range node.Values {
		children = append(children, value)
	}
	return children
}

// CheckResourceLimits checks r against the decoder's resource policy without
// decoding it into Go values.
//
// Only the AST reference graph is traversed: syntax nodes are counted, alias
// references are resolved by name, the longest nested alias chain is measured
// and cyclic alias references are reported. Alias targets are never expanded
// or cloned, so MaxLogicalNodes is not enforced by this check-only mode.
//
// When r is the decoder's own input it is buffered, so the decoder can still
// be used to decode the input after a successful check. Limits are isolated to
// the decoder instance.
func (d *Decoder) CheckResourceLimits(r io.Reader) error {
	return d.CheckResourceLimitsContext(context.Background(), r)
}

// CheckResourceLimitsContext is like [Decoder.CheckResourceLimits] but accepts
// a context.
func (d *Decoder) CheckResourceLimitsContext(ctx context.Context, r io.Reader) error {
	_ = ctx
	if err := d.resourceLimits.validate(); err != nil {
		return err
	}
	if !d.resourceLimits.enabled() {
		return nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if r == d.reader {
		// Preserve the decoder input for subsequent Decode calls.
		d.reader = bytes.NewReader(data)
	}
	file, err := d.parseForResourceCheck(data)
	if err != nil {
		return err
	}
	return newResourceChecker(d.resourceLimits).check(file, false)
}

func (d *Decoder) parseForResourceCheck(data []byte) (*ast.File, error) {
	var parseMode parser.Mode
	if d.toCommentMap != nil {
		parseMode = parser.ParseComments
	}
	var opts []parser.Option
	if d.allowDuplicateMapKey {
		opts = append(opts, parser.AllowDuplicateMapKey())
	}
	return parser.ParseBytes(data, parseMode, opts...)
}

// CheckResourceLimits checks r against limits without decoding it into Go
// values. It is a shorthand for configuring a decoder with
// [WithResourceLimits] and calling [Decoder.CheckResourceLimits]; see that
// method for the check-only semantics.
func CheckResourceLimits(r io.Reader, limits ResourceLimits) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	reader := bytes.NewReader(data)
	d := NewDecoder(reader)
	if err := WithResourceLimits(limits)(d); err != nil {
		return err
	}
	return d.CheckResourceLimits(reader)
}
