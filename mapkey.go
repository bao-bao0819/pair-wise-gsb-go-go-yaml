package yaml

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/internal/errors"
	"github.com/goccy/go-yaml/token"
)

// mapKeyKind is the resolved type family of a mapping key.
// Keys of different kinds are never considered equal, even if their
// textual or numeric values look the same (e.g. string "1" and integer 1).
type mapKeyKind int

const (
	mapKeyNull mapKeyKind = iota
	mapKeyString
	mapKeyBool
	mapKeyInt
	mapKeyUint
	mapKeyFloat
	mapKeyNaN
	mapKeyBinary
	mapKeyTimestamp
)

// canonicalMapKey is the resolved identity of a mapping key.
// Two keys are duplicates if and only if their canonical keys are equal.
type canonicalMapKey struct {
	kind mapKeyKind
	str  string
	b    bool
	i64  int64
	u64  uint64
	f64  float64
}

// canonicalizeMapKey converts a resolved key value into its canonical form.
// It returns false for values that have no defined equivalence
// (e.g. sequences or mappings used as keys); such keys are excluded
// from the uniqueness check instead of being compared by Go map semantics.
func canonicalizeMapKey(value interface{}) (canonicalMapKey, bool) {
	switch v := value.(type) {
	case nil:
		return canonicalMapKey{kind: mapKeyNull}, true
	case string:
		return canonicalMapKey{kind: mapKeyString, str: v}, true
	case bool:
		return canonicalMapKey{kind: mapKeyBool, b: v}, true
	case int:
		return canonicalIntMapKey(int64(v)), true
	case int8:
		return canonicalIntMapKey(int64(v)), true
	case int16:
		return canonicalIntMapKey(int64(v)), true
	case int32:
		return canonicalIntMapKey(int64(v)), true
	case int64:
		return canonicalIntMapKey(v), true
	case uint:
		return canonicalMapKey{kind: mapKeyUint, u64: uint64(v)}, true
	case uint8:
		return canonicalMapKey{kind: mapKeyUint, u64: uint64(v)}, true
	case uint16:
		return canonicalMapKey{kind: mapKeyUint, u64: uint64(v)}, true
	case uint32:
		return canonicalMapKey{kind: mapKeyUint, u64: uint64(v)}, true
	case uint64:
		return canonicalMapKey{kind: mapKeyUint, u64: v}, true
	case float32:
		return canonicalFloatMapKey(float64(v)), true
	case float64:
		return canonicalFloatMapKey(v), true
	case []byte:
		// !!binary keys are compared by their decoded bytes.
		return canonicalMapKey{kind: mapKeyBinary, str: string(v)}, true
	case time.Time:
		// !!timestamp keys are compared by the instant they denote.
		return canonicalMapKey{kind: mapKeyTimestamp, str: v.UTC().Format(time.RFC3339Nano)}, true
	default:
		return canonicalMapKey{}, false
	}
}

// canonicalIntMapKey normalizes signed integers so that non-negative values
// share the same identity as their unsigned counterparts: 1, 0x1, 0o1 and +1
// are all the same key, while -1 is distinct from any unsigned value.
func canonicalIntMapKey(v int64) canonicalMapKey {
	if v >= 0 {
		return canonicalMapKey{kind: mapKeyUint, u64: uint64(v)}
	}
	return canonicalMapKey{kind: mapKeyInt, i64: v}
}

func canonicalFloatMapKey(v float64) canonicalMapKey {
	if math.IsNaN(v) {
		// All NaN keys are considered equal to each other.
		return canonicalMapKey{kind: mapKeyNaN}
	}
	if v == 0 {
		// Negative zero and positive zero are the same key.
		v = 0
	}
	return canonicalMapKey{kind: mapKeyFloat, f64: v}
}

// validateMapKeyUniqueness reports explicitly duplicated keys within a single
// mapping when DisallowDuplicateMapKeyResolved is enabled. Keys introduced by
// merge keys (<<) are governed by the merge rules and are not compared against
// explicit keys; explicit duplicates inside each merged mapping are reported
// in their own scope.
func (d *Decoder) validateMapKeyUniqueness(ctx context.Context, mapNode ast.MapNode) error {
	if !d.disallowDuplicateMapKeyResolved || d.allowDuplicateMapKey {
		return nil
	}
	d.stepIn()
	defer d.stepOut()
	if d.isExceededMaxDepth() {
		return ErrExceededMaxDepth
	}

	seen := map[canonicalMapKey]ast.Node{}
	mapIter := mapNode.MapRange()
	for mapIter.Next() {
		keyNode := mapIter.Key()
		if keyNode.IsMergeKey() {
			if err := d.validateMergeSourceKeyUniqueness(ctx, mapIter.Value()); err != nil {
				return err
			}
			continue
		}
		key, ok, err := d.resolvedMapKey(ctx, keyNode)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if first, exists := seen[key]; exists {
			return newDuplicateMapKeyError(first, keyNode)
		}
		seen[key] = keyNode
	}
	return nil
}

// validateMergeSourceKeyUniqueness validates the explicit keys of every mapping
// introduced by a merge value. Each merge source is validated in its own scope;
// conflicts between merge sources or with explicit keys are merge-rule matters
// and are not reported here.
func (d *Decoder) validateMergeSourceKeyUniqueness(ctx context.Context, node ast.Node) error {
	switch n := node.(type) {
	case *ast.AnchorNode:
		return d.validateMergeSourceKeyUniqueness(ctx, n.Value)
	case *ast.AliasNode:
		target := d.anchorNodeMap[n.Value.GetToken().Value]
		if target == nil {
			return nil
		}
		return d.validateMergeSourceKeyUniqueness(ctx, target)
	case *ast.TagNode:
		return d.validateMergeSourceKeyUniqueness(ctx, n.Value)
	case *ast.SequenceNode:
		for _, item := range n.Values {
			if err := d.validateMergeSourceKeyUniqueness(ctx, item); err != nil {
				return err
			}
		}
		return nil
	case ast.MapNode:
		return d.validateMapKeyUniqueness(ctx, n)
	}
	return nil
}

// resolvedMapKey resolves a mapping key node under the decoder's scalar
// resolution rules (including explicit tags) and returns its canonical identity.
func (d *Decoder) resolvedMapKey(ctx context.Context, keyNode ast.Node) (canonicalMapKey, bool, error) {
	value, err := d.nodeToValue(ctx, keyNode)
	if err != nil {
		return canonicalMapKey{}, false, err
	}
	key, ok := canonicalizeMapKey(value)
	return key, ok, nil
}

func newDuplicateMapKeyError(first, dup ast.Node) *errors.DuplicateKeyError {
	firstTk := first.GetToken()
	dupTk := dup.GetToken()
	var line, column int
	if firstTk != nil && firstTk.Position != nil {
		line = firstTk.Position.Line
		column = firstTk.Position.Column
	}
	err := errors.ErrDuplicateKey(
		fmt.Sprintf(
			"duplicate key %q: key %q with the same resolved value is already declared at [%d:%d]",
			mapKeySpelling(dup), mapKeySpelling(first), line, column,
		),
		dupTk,
	)
	err.FirstToken = firstTk
	return err
}

// mapKeySpelling returns the original source spelling of a mapping key,
// e.g. 0x1, 'a' or !!str 1, without re-serializing the node.
func mapKeySpelling(node ast.Node) string {
	switch n := node.(type) {
	case *ast.MappingKeyNode:
		return mapKeySpelling(n.Value)
	case *ast.TagNode:
		return strings.TrimSpace(tokenSpelling(n.Start) + " " + mapKeySpelling(n.Value))
	case *ast.AnchorNode:
		return strings.TrimSpace(tokenSpelling(n.GetToken()) + " " + mapKeySpelling(n.Value))
	}
	return tokenSpelling(node.GetToken())
}

func tokenSpelling(tk *token.Token) string {
	if tk == nil {
		return ""
	}
	if origin := strings.TrimSpace(tk.Origin); origin != "" {
		return origin
	}
	return tk.Value
}
