package yaml_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/internal/errors"
)

func TestDecoder_UniqueMapKeys(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
		keyText string
	}{
		{
			name:    "literal duplicate",
			source:  "a: 1\na: 2\n",
			wantErr: true,
			keyText: "a",
		},
		{
			name:    "explicit !!str equal to plain bareword string",
			source:  "!!str foo: a\nfoo: b\n",
			wantErr: true,
			keyText: "foo",
		},
		{
			name:   "explicit !!str distinct from integer",
			source: "!!str 1: a\n1: b\n",
		},
		{
			name:    "integers with different bases are equal",
			source:  "0x10: a\n16: b\n",
			wantErr: true,
			keyText: "16",
		},
		{
			name:   "string 1 and integer 1 are distinct",
			source: "\"1\": a\n1: b\n",
		},
		{
			name:   "integer and float are distinct",
			source: "1: a\n1.0: b\n",
		},
		{
			name:    "negative zero duplicate",
			source:  "-0.0: a\n0.0: b\n",
			wantErr: true,
			keyText: "0.0",
		},
		{
			name:   "repeated nan is allowed",
			source: ".nan: a\n.nan: b\n",
		},
		{
			name:    "explicit !!bool duplicate",
			source:  "!!bool \"true\": a\ntrue: b\n",
			wantErr: true,
			keyText: "true",
		},
		{
			name:    "key containing a dot",
			source:  "a.b: 1\na.b: 2\n",
			wantErr: true,
			keyText: "a.b",
		},
		{
			name:    "nested mapping duplicate",
			source:  "outer:\n  inner: 1\n  inner: 2\n",
			wantErr: true,
			keyText: "inner",
		},
		{
			name:   "same key at different nesting levels",
			source: "inner: 1\nouter:\n  inner: 2\n",
		},
		{
			name:   "merge override is allowed",
			source: "base: &b\n  x: 1\ntop:\n  <<: *b\n  x: 2\n",
		},
		{
			name:    "explicit duplicate inside merged mapping is reported",
			source:  "top:\n  <<: {x: 1, x: 2}\n",
			wantErr: true,
			keyText: "x",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var v interface{}
			err := yaml.UnmarshalWithOptions([]byte(test.source), &v, yaml.UniqueMapKeys())
			if !test.wantErr {
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
				t.Fatalf("expected *yaml.UniqueMapKeyError but got %T: %v", err, err)
			}
			if uniqueErr.GetFirstToken() == nil || uniqueErr.GetToken() == nil {
				t.Fatal("both the first and the duplicate token must be present")
			}
			if uniqueErr.GetFirstToken() == uniqueErr.GetToken() {
				t.Fatal("the first token and the duplicate token must differ")
			}
			message := uniqueErr.GetMessage()
			if !strings.Contains(message, strconv.Quote(test.keyText)) {
				t.Fatalf("error %q does not preserve original key spelling %q", message, test.keyText)
			}
			if !strings.Contains(message, "already defined at") {
				t.Fatalf("error %q must reference the first declaration", message)
			}
		})
	}
}

func TestDecoder_UniqueMapKeys_LenientDefault(t *testing.T) {
	// The default decode mode stays lenient: keys that are semantically equal
	// but written differently are accepted and collapse to one value.
	source := "0x10: a\n16: b\n"
	var v map[int]string
	if err := yaml.Unmarshal([]byte(source), &v); err != nil {
		t.Fatalf("default mode must stay compatible, got %v", err)
	}
}

func TestDecoder_UniqueMapKeys_AllowDuplicateOverrides(t *testing.T) {
	// AllowDuplicateMapKey keeps its role of disabling duplicate detection;
	// the semantic checker does not run when it is requested.
	source := "a: 1\na: 2\n"
	var v map[string]string
	if err := yaml.UnmarshalWithOptions([]byte(source), &v, yaml.UniqueMapKeys(), yaml.AllowDuplicateMapKey()); err != nil {
		t.Fatalf("AllowDuplicateMapKey should suppress the error, got %v", err)
	}
}
