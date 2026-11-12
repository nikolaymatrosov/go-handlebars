package handlebars

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() {
	specPrintAST = func(src string) (string, error) {
		prog, err := parse(src, Options{})
		if err != nil {
			return "", err
		}
		return printAST(prog), nil
	}
}

func TestParseDepthLimit(t *testing.T) {
	cases := map[string]string{
		"blocks":  strings.Repeat("{{#if x}}", 2000) + strings.Repeat("{{/if}}", 2000),
		"sexprs":  "{{foo " + strings.Repeat("(foo ", 2000) + strings.Repeat(")", 2000) + "}}",
		"chain":   "{{#if a}}" + strings.Repeat("{{else if b}}", 2000) + "{{/if}}",
		"partial": strings.Repeat("{{#> p}}", 2000) + strings.Repeat("{{/p}}", 2000),
	}
	for name, src := range cases {
		_, err := parse(src, Options{})
		var le *LimitError
		if !errors.As(err, &le) || le.What != "parse depth" || le.Max != DefaultLimits.MaxParseDepth {
			t.Errorf("%s: got %v, want parse depth LimitError", name, err)
		}
	}
	if _, err := parse(strings.Repeat("{{#if x}}", 100)+strings.Repeat("{{/if}}", 100), Options{}); err != nil {
		t.Errorf("depth 100 should parse: %v", err)
	}
}

// Cheap pre-fuzz check: no spec template may panic the parser.
func TestParseSpecTemplatesNoPanic(t *testing.T) {
	paths, _ := filepath.Glob("handlebars-spec/spec/*.json")
	if len(paths) == 0 {
		t.Fatal("no spec files")
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var tests []struct{ Template string }
		if err := json.Unmarshal(raw, &tests); err != nil {
			t.Fatal(p, err)
		}
		for _, tc := range tests {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s: parse(%q) panicked: %v", p, tc.Template, r)
					}
				}()
				parse(tc.Template, Options{}) // errors are fine
			}()
		}
	}
}

func TestParseErrorText(t *testing.T) {
	cases := map[string]string{
		"{{#hellos}}{{text/this/foo}}{{/hellos}}":     "Invalid path: text/this - 1:13",
		"{{#hellos}}{{foo text/this/foo}}{{/hellos}}": "Invalid path: text/this - 1:17",
		"{{#goodbyes}}{{/hellos}}":                    "goodbyes doesn't match hellos - 1:3",
		"{{#* foo}}{{^}}{{/foo}}":                     "Unexpected inverse block on decorator",
		"{{foo}":                                      "Parse error on line 1:\n{{foo}\n-----^\nExpecting ",
		"a\n{{foo/../bar}":                            "Parse error on line 2:\na{{foo/../bar}\n-------------^\nExpecting ",
	}
	for src, want := range cases {
		_, err := parse(src, Options{})
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("parse(%q) = %v, want prefix %q", src, err, want)
		}
	}
}

func TestWhitespaceControl(t *testing.T) {
	cases := []struct {
		src    string
		ignore bool
		want   string
	}{
		{"a {{~foo~}} b", false, "CONTENT[ 'a' ]\n{{ PATH:foo [] }}\nCONTENT[ 'b' ]\n"},
		{"  {{> foo}}\n", false, "CONTENT[ '' ]\n{{> PARTIAL:foo }}\nCONTENT[ '' ]\n"},
		{"  {{> foo}}\n", true, "CONTENT[ '  ' ]\n{{> PARTIAL:foo }}\nCONTENT[ '\n' ]\n"},
		{"{{#a}}\n  x\n{{/a}}\n", false, "BLOCK:\n  PATH:a []\n  PROGRAM:\n    CONTENT[ '  x\n' ]\nCONTENT[ '' ]\n"},
	}
	for _, c := range cases {
		prog, err := parse(c.src, Options{IgnoreStandalone: c.ignore})
		if err != nil {
			t.Fatal(err)
		}
		if got := printAST(prog); got != c.want {
			t.Errorf("parse(%q, ignore=%v):\n got %q\nwant %q", c.src, c.ignore, got, c.want)
		}
	}
	prog, _ := parse("  {{> foo}}\n", Options{})
	if pn := prog.body[1].(*partialNode); pn.indent != "  " {
		t.Errorf("partial indent = %q, want %q", pn.indent, "  ")
	}
}
