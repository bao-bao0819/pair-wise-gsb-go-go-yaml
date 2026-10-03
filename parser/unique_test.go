package parser_test

import (
	"strconv"
	"testing"

	"github.com/goccy/go-yaml/internal/errors"
	"github.com/goccy/go-yaml/parser"
)

func TestParser_UniqueMapKeys(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		err       bool
		firstLine int
		firstCol  int
		dupLine   int
		dupCol    int
		keyText   string
	}{
		{
			name:      "literal duplicates are still reported",
			source:    "a: 1\na: 2\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "a",
		},
		{
			name:      "explicit !!str is a string key equal to quoted string",
			source:    "!!str 1: a\n\"1\": b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   `"1"`,
		},
		{
			name:   "explicit !!str is not equal to integer 1",
			source: "!!str 1: a\n1: b\n",
		},
		{
			name:   "plain quoted string is not equal to integer",
			source: "\"1\": a\n1: b\n",
		},
		{
			name:      "bareword key equals explicit !!str key",
			source:    "foo: a\n!!str foo: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "!!str foo",
		},
		{
			name:      "hexadecimal equals decimal integer",
			source:    "0x10: a\n16: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "16",
		},
		{
			name:      "binary octal hexadecimal and decimal integers are equal",
			source:    "0b10000: a\n0o20: b\n16: c\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "0o20",
		},
		{
			name:      "underscored integer equals decimal integer",
			source:    "1_000: a\n1000: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "1000",
		},
		{
			name:      "explicit !!int participates in integer resolution",
			source:    "? !!int 5\n: a\n0x5: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   3,
			dupCol:    1,
			keyText:   "0x5",
		},
		{
			name:      "integers beyond int64 keep an exact identity",
			source:    "9223372036854775808: a\n0x8000000000000000: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "0x8000000000000000",
		},
		{
			name:   "integer is not equal to float",
			source: "1: a\n1.0: b\n",
		},
		{
			name:      "floats with different spelling are equal by value",
			source:    "1.0: a\n1.00: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "1.00",
		},
		{
			name:      "negative zero equals positive zero",
			source:    "-0.0: a\n0.0: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "0.0",
		},
		{
			name:   "nan is equal to nothing including another nan",
			source: ".nan: a\n.nan: b\n.NaN: c\n",
		},
		{
			name:      "positive infinities are equal",
			source:    ".inf: a\n.Inf: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   ".Inf",
		},
		{
			name:   "positive and negative infinity are distinct",
			source: ".inf: a\n-.inf: b\n",
		},
		{
			name:      "boolean spellings are equal",
			source:    "true: a\nTrue: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "True",
		},
		{
			name:      "explicit !!bool equals plain boolean",
			source:    "!!bool \"true\": a\ntrue: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "true",
		},
		{
			name:      "null spellings are equal",
			source:    "null: a\n~: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "~",
		},
		{
			name:      "explicit !!null equals plain null",
			source:    "!!null x: a\nnull: b\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "null",
		},
		{
			name:      "tagged scalars with same tag and text are equal",
			source:    "!!binary a: x\n!!binary a: y\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "!!binary a",
		},
		{
			name:   "tagged scalars with different text are distinct",
			source: "!!binary a: x\n!!binary b: y\n",
		},
		{
			name:   "tagged scalar is not equal to an untagged scalar",
			source: "!!timestamp 2020-01-01: x\n2020-01-01: y\n",
		},
		{
			name:   "invalid !!int scalar is not compared as an integer",
			source: "!!int foo: a\nfoo: b\n",
		},
		{
			name:      "nested mapping duplicate is reported at its own location",
			source:    "a:\n  b: 1\n  b: 2\n",
			err:       true,
			firstLine: 2,
			firstCol:  3,
			dupLine:   3,
			dupCol:    3,
			keyText:   "b",
		},
		{
			name:   "same key at different mapping levels is allowed",
			source: "b: 1\na:\n  b: 2\n",
		},
		{
			name:      "key containing a dot keeps its literal key spelling",
			source:    "a.b: 1\na.b: 2\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "a.b",
		},
		{
			name:      "flow mapping duplicate is reported",
			source:    "{1: a, 0x1: b}\n",
			err:       true,
			firstLine: 1,
			firstCol:  2,
			dupLine:   1,
			dupCol:    8,
			keyText:   "0x1",
		},
		{
			name:   "merge override is not an explicit duplicate declaration",
			source: "x: 0\n<<: {x: 1}\n",
		},
		{
			name:      "duplicate inside a merged mapping is still explicit",
			source:    "<<: {x: 1, x: 2}\n",
			err:       true,
			firstLine: 1,
			firstCol:  6,
			dupLine:   1,
			dupCol:    12,
			keyText:   "x",
		},
		{
			name:      "anchor does not change key identity",
			source:    "&a 1: x\n1: y\n",
			err:       true,
			firstLine: 1,
			firstCol:  1,
			dupLine:   2,
			dupCol:    1,
			keyText:   "1",
		},
		{
			name:   "alias keys are not comparable and skipped",
			source: "a: &v 1\n? *v\n: x\n? *v\n: y\n",
		},
		{
			name:   "string and integer keys of unrelated values are accepted",
			source: "a: 1\n2: b\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parser.ParseBytes([]byte(test.source), 0, parser.UniqueMapKeys())
			if !test.err {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected duplicate key error, but got nil")
			}
			var uniqueErr *errors.UniqueMapKeyError
			if !errors.As(err, &uniqueErr) {
				t.Fatalf("expected *errors.UniqueMapKeyError but got %T: %v", err, err)
			}
			first := uniqueErr.GetFirstToken().Position
			dup := uniqueErr.GetToken().Position
			if first.Line != test.firstLine || first.Column != test.firstCol {
				t.Errorf("first position = [%d:%d], want [%d:%d]", first.Line, first.Column, test.firstLine, test.firstCol)
			}
			if dup.Line != test.dupLine || dup.Column != test.dupCol {
				t.Errorf("duplicate position = [%d:%d], want [%d:%d]", dup.Line, dup.Column, test.dupLine, test.dupCol)
			}
			want := "mapping key " + strconv.Quote(test.keyText) + " already defined at [" +
				strconv.Itoa(test.firstLine) + ":" + strconv.Itoa(test.firstCol) + "]"
			if uniqueErr.GetMessage() != want {
				t.Errorf("message = %q, want %q", uniqueErr.GetMessage(), want)
			}
		})
	}
}

func TestParser_UniqueMapKeys_DefaultModeUnaffected(t *testing.T) {
	// Semantically equal but literally distinct keys keep the lenient default
	// behavior of parsing successfully.
	sources := []string{
		"0x10: a\n17: b\n",
		"!!str 1: a\n\"2\": b\n",
		"1: a\n1.0: b\n",
	}
	for _, source := range sources {
		if _, err := parser.ParseBytes([]byte(source), 0); err != nil {
			t.Fatalf("default parse mode rejected %q: %v", source, err)
		}
	}
}
