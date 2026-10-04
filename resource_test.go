package yaml_test

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

// exponentialBomb builds a document with 2*n+3 flow-style syntax lines whose
// fully expanded logical size grows as 2^(n+2)-1. Each anchor refers to the
// previous one twice.
func exponentialBomb(n int) string {
	var b strings.Builder
	b.WriteString("a: &a [0, 0]\n")
	prev := "a"
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("n%d", i)
		fmt.Fprintf(&b, "%s: &%s [*%s, *%s]\n", name, name, prev, prev)
		prev = name
	}
	fmt.Fprintf(&b, "result: *%s\n", prev)
	return b.String()
}

func TestResourceLimits_ExponentialBombRejectedQuickly(t *testing.T) {
	src := exponentialBomb(30)
	limits := yaml.ResourceLimits{
		MaxSyntaxNodes:  1000,
		MaxAliasDepth:   100,
		MaxLogicalNodes: 100000,
	}

	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	before := ms.TotalAlloc

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		var v any
		done <- yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimits(limits)).Decode(&v)
	}()
	select {
	case err := <-done:
		if !yaml.IsResourceLimitError(err, yaml.LogicalNodeLimit) {
			t.Fatalf("expected LogicalNodeLimit error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decode of exponential bomb was not rejected in time")
	}
	elapsed := time.Since(start)

	runtime.ReadMemStats(&ms)
	allocated := ms.TotalAlloc - before
	if allocated > 64<<20 {
		t.Fatalf("rejection allocated too much: %d bytes", allocated)
	}
	t.Logf("rejected in %s, allocated %d bytes", elapsed, allocated)
}

func TestResourceLimits_ExponentialBombTypedTarget(t *testing.T) {
	// The normalization pass eagerly expands aliases into Go containers; the
	// policy must reject before those containers are built.
	src := exponentialBomb(24)
	limits := yaml.ResourceLimits{MaxLogicalNodes: 1 << 20}
	type doc struct {
		A      []any
		Result []any
	}
	var v doc
	err := yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimits(limits)).Decode(&v)
	if !yaml.IsResourceLimitError(err, yaml.LogicalNodeLimit) {
		t.Fatalf("expected LogicalNodeLimit error, got %v", err)
	}
}

func TestResourceLimits_SharedAnchorCachesWeightButCountsEachReference(t *testing.T) {
	var b strings.Builder
	b.WriteString("base: &base {x: 1, y: 2}\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "v%d: *base\n", i)
	}
	src := b.String()

	// 50 references to the same 5-node anchor plus the anchor definition and
	// 51 mapping entries stay well within a generous budget and decode fine.
	generous := yaml.ResourceLimits{
		MaxSyntaxNodes:  100000,
		MaxAliasDepth:   10,
		MaxLogicalNodes: 100000,
	}
	var v any
	if err := yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimits(generous)).Decode(&v); err != nil {
		t.Fatalf("legitimate repeated anchors must decode: %v", err)
	}

	// A tight logical budget rejects because every one of the 50 references
	// contributes the cached anchor weight, even though the source is small.
	tight := yaml.ResourceLimits{MaxLogicalNodes: 100}
	err := yaml.NewDecoder(strings.NewReader(src), yaml.WithResourceLimits(tight)).Decode(&v)
	if !yaml.IsResourceLimitError(err, yaml.LogicalNodeLimit) {
		t.Fatalf("expected LogicalNodeLimit error, got %v", err)
	}
}

func TestResourceLimits_ExceedingPathReported(t *testing.T) {
	var b strings.Builder
	b.WriteString("a: &a\n  k: 1\n")
	prev := "a"
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&b, "%s: &%s\n  ref: *%s\n", name, name, prev)
		prev = name
	}
	fmt.Fprintf(&b, "r: *%s\n", prev)

	err := yaml.CheckResourceLimits(strings.NewReader(b.String()), yaml.ResourceLimits{MaxAliasDepth: 3})
	if !yaml.IsResourceLimitError(err, yaml.AliasDepthLimit) {
		t.Fatalf("expected AliasDepthLimit error, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"*c5", "*c2", "*a"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing path hop %s", msg, want)
		}
	}
}

func TestResourceLimits_SyntaxNodeLimit(t *testing.T) {
	src := strings.Repeat("- value\n", 100)
	err := yaml.CheckResourceLimits(strings.NewReader(src), yaml.ResourceLimits{MaxSyntaxNodes: 50})
	if !yaml.IsResourceLimitError(err, yaml.SyntaxNodeLimit) {
		t.Fatalf("expected SyntaxNodeLimit error, got %v", err)
	}
}

func TestResourceLimits_CycleReportedSeparately(t *testing.T) {
	t.Run("mutual", func(t *testing.T) {
		src := "a: &a\n  x: *b\nb: &b\n  y: *a\nr: *a\n"
		err := yaml.CheckResourceLimits(
			strings.NewReader(src), yaml.ResourceLimits{MaxAliasDepth: 100000},
		)
		if !yaml.IsAliasCycleError(err) {
			t.Fatalf("expected AliasCycleError, got %v", err)
		}
		if yaml.IsResourceLimitError(err, yaml.AliasDepthLimit) {
			t.Fatalf("cycle must not be reported as depth limit: %v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "*a -> *b -> *a") {
			t.Fatalf("unexpected cycle path in %q", msg)
		}
	})
	t.Run("self", func(t *testing.T) {
		src := "a: &a\n  x: *a\nr: *a\n"
		err := yaml.CheckResourceLimits(
			strings.NewReader(src), yaml.ResourceLimits{MaxAliasDepth: 100000},
		)
		if !yaml.IsAliasCycleError(err) {
			t.Fatalf("expected AliasCycleError, got %v", err)
		}
	})
	t.Run("decode mode", func(t *testing.T) {
		src := "a: &a\n  x: *b\nb: &b\n  y: *a\nr: *a\n"
		var v any
		err := yaml.NewDecoder(
			strings.NewReader(src),
			yaml.WithResourceLimits(yaml.ResourceLimits{MaxLogicalNodes: 1 << 30}),
		).Decode(&v)
		if !yaml.IsAliasCycleError(err) {
			t.Fatalf("expected AliasCycleError during decode, got %v", err)
		}
	})
}

func TestResourceLimits_CheckModeDoesNotExpand(t *testing.T) {
	// Check-only traversal must not reject on logical size and must not fail
	// on an alias that cannot be resolved in the document (forward reference).
	src := exponentialBomb(30)
	err := yaml.CheckResourceLimits(
		strings.NewReader(src),
		yaml.ResourceLimits{MaxSyntaxNodes: 100000, MaxAliasDepth: 100000, MaxLogicalNodes: 10},
	)
	if err != nil {
		t.Fatalf("check-only mode must ignore MaxLogicalNodes: %v", err)
	}

	forward := "r: *later\nlater: &later 1\n"
	err = yaml.CheckResourceLimits(strings.NewReader(forward), yaml.ResourceLimits{MaxSyntaxNodes: 1000})
	if err != nil {
		t.Fatalf("check-only mode must not fail on forward alias: %v", err)
	}
}

func TestResourceLimits_CheckThenDecodeReusesInput(t *testing.T) {
	src := "a: 1\nb: [2, 3]\n"
	reader := strings.NewReader(src)
	d := yaml.NewDecoder(
		reader,
		yaml.WithResourceLimits(yaml.ResourceLimits{MaxSyntaxNodes: 100}),
	)
	if err := d.CheckResourceLimits(reader); err != nil {
		t.Fatalf("unexpected check error: %v", err)
	}
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode after check must work: %v", err)
	}
	if got := v["a"]; fmt.Sprint(got) != "1" {
		t.Fatalf("unexpected value: %#v", got)
	}
}

func TestResourceLimits_IsolatedPerDecoder(t *testing.T) {
	limited := yaml.NewDecoder(
		strings.NewReader("x: 1\n"),
		yaml.WithResourceLimits(yaml.ResourceLimits{MaxSyntaxNodes: 1}),
	)
	if err := limited.Decode(&map[string]any{}); err == nil {
		t.Fatal("limited decoder should reject")
	}

	unlimited := yaml.NewDecoder(strings.NewReader(strings.Repeat("- v\n", 100)))
	var v any
	if err := unlimited.Decode(&v); err != nil {
		t.Fatalf("decoder without limits must keep default behavior: %v", err)
	}
}

func TestResourceLimits_NegativeLimitRejected(t *testing.T) {
	var v any
	err := yaml.NewDecoder(
		strings.NewReader("x: 1\n"),
		yaml.WithResourceLimits(yaml.ResourceLimits{MaxLogicalNodes: -1}),
	).Decode(&v)
	if err == nil {
		t.Fatal("negative limit must be rejected")
	}
}

func TestResourceLimits_DefaultBehaviorUnchanged(t *testing.T) {
	src := exponentialBomb(20)
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("default decoding must not enforce limits: %v", err)
	}
}

func TestResourceLimits_CheckNoLimitsIsNoop(t *testing.T) {
	r := io.Reader(strings.NewReader("x: 1\n"))
	if err := yaml.NewDecoder(r).CheckResourceLimits(r); err != nil {
		t.Fatalf("check without limits must be a no-op: %v", err)
	}
}

func TestResourceLimits_LargeLegalRepeatedAnchor(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("base: &base {a: 1, b: 2, c: 3}\n")
	const refs = 500
	for i := 0; i < refs; i++ {
		fmt.Fprintf(&b, "k%d: *base\n", i)
	}
	limits := yaml.ResourceLimits{
		MaxSyntaxNodes:  100000,
		MaxAliasDepth:   10,
		MaxLogicalNodes: 100000,
	}
	var v any
	if err := yaml.NewDecoder(&b, yaml.WithResourceLimits(limits)).Decode(&v); err != nil {
		t.Fatalf("legal repeated anchors within budget must decode: %v", err)
	}
	m := v.(map[string]any)
	if len(m) != refs+1 {
		t.Fatalf("unexpected number of keys: %d", len(m))
	}
}
