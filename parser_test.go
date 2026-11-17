package handlebars

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
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

// Cheap pre-fuzz check: no spec template may panic the parser.
func TestParseSpecTemplatesNoPanic(t *testing.T) {
	for _, p := range specPaths(t) {
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

// Parsing must stay linear in the source: each case sits near
// MaxTemplateSize and took seconds to minutes when every path scanned the
// enclosing block params (a long `as |...|` list, or many nested programs
// under a raised MaxParseDepth).
func TestParseLinear(t *testing.T) {
	n := DefaultLimits.MaxTemplateSize
	deep := strings.Repeat("{{#each a as |x|}}", n/80) + strings.Repeat("{{y}}", n/10) + strings.Repeat("{{/each}}", n/80)
	cases := map[string]string{
		"long list": "{{#each a as |" + strings.Repeat("a ", n/5) + "|}}" + strings.Repeat("{{b}}", n/10) + "{{/each}}",
		"deep":      deep,
	}
	for name, src := range cases {
		start := time.Now()
		_, err := ParseWithOptions(src, Options{Limits: Limits{MaxParseDepth: n, MaxExpressions: n}})
		d := time.Since(start)
		t.Logf("%s (%d KiB): %v", name, len(src)>>10, d)
		if err != nil || d > time.Second {
			t.Errorf("%s: %v, %v; want a linear parse", name, d, err)
		}
	}
}

// printAST renders p exactly like handlebars.js compiler/printer.js,
// including its padding drift (Program decrements padding on exit).
func printAST(p *program) string {
	var pr printer
	return pr.program(p)
}

type printer struct{ padding int }

func (pr *printer) pad(s string) string {
	return strings.Repeat("  ", max(pr.padding, 0)) + s + "\n"
}

func (pr *printer) program(p *program) string {
	var b strings.Builder
	if p.blockParams != nil {
		b.WriteString(pr.pad("BLOCK PARAMS: [ " + strings.Join(p.blockParams, " ") + " ]"))
	}
	for _, n := range p.body {
		b.WriteString(pr.accept(n))
	}
	pr.padding--
	return b.String()
}

func (pr *printer) accept(n node) string {
	switch n := n.(type) {
	case *contentNode:
		return pr.pad("CONTENT[ '" + n.value + "' ]")
	case *commentNode:
		return pr.pad("{{! '" + n.value + "' }}")
	case *mustacheNode:
		if n.decorator {
			return pr.pad("{{ DIRECTIVE " + pr.call(&n.call) + " }}")
		}
		return pr.pad("{{ " + pr.call(&n.call) + " }}")
	case *blockNode:
		return pr.block(n)
	case *partialNode:
		return pr.partial(n)
	case *subExpr:
		return pr.call(&n.call)
	case *pathExpr:
		s := "PATH:" + strings.Join(n.parts, "/")
		if n.data {
			s = "@" + s
		}
		return s
	case *strLit:
		return `"` + n.value + `"`
	case *numLit:
		return "NUMBER{" + string(appendJSNumber(nil, n.value)) + "}"
	case *boolLit:
		return "BOOLEAN{" + strconv.FormatBool(n.value) + "}"
	case *undefinedLit:
		return "UNDEFINED"
	case *nullLit:
		return "NULL"
	}
	return ""
}

func (pr *printer) block(n *blockNode) string {
	out := "BLOCK:"
	if n.decorator {
		out = "DIRECTIVE BLOCK:"
	}
	out = pr.pad(out)
	pr.padding++
	out += pr.pad(pr.call(&n.call))
	if n.program != nil {
		out += pr.pad("PROGRAM:")
		pr.padding++
		out += pr.program(n.program)
		pr.padding--
	}
	if n.inverse != nil {
		if n.program != nil {
			pr.padding++
		}
		out += pr.pad("{{^}}")
		pr.padding++
		out += pr.program(n.inverse)
		pr.padding--
		if n.program != nil {
			pr.padding--
		}
	}
	pr.padding--
	return out
}

func (pr *printer) partial(n *partialNode) string {
	orig, _ := jsOriginal(n.name)
	content := "PARTIAL:" + orig
	if n.program != nil {
		content = "PARTIAL BLOCK:" + orig
	}
	if len(n.params) > 0 {
		content += " " + pr.accept(n.params[0])
	}
	if n.hash != nil {
		content += " " + pr.hash(n.hash)
	}
	if n.program != nil {
		content += " " + pr.pad("PROGRAM:")
		pr.padding++
		content += pr.program(n.program)
		pr.padding--
	}
	return pr.pad("{{> " + content + " }}")
}

func (pr *printer) call(c *call) string {
	params := make([]string, len(c.params))
	for i, p := range c.params {
		params[i] = pr.accept(p)
	}
	s := pr.accept(c.path) + " [" + strings.Join(params, ", ") + "]"
	if c.hash != nil {
		s += " " + pr.hash(c.hash)
	}
	return s
}

func (pr *printer) hash(h []hashPair) string {
	pairs := make([]string, len(h))
	for i, kv := range h {
		pairs[i] = kv.key + "=" + pr.accept(kv.value)
	}
	return "HASH{" + strings.Join(pairs, ", ") + "}"
}

// NoBlocks rejects every block statement at its opening tag; everything
// else parses and renders as without it.
func TestNoBlocks(t *testing.T) {
	noBlocks := Options{NoBlocks: true}
	rejected := []struct {
		src       string
		line, col int
	}{
		{"{{#if a}}x{{/if}}", 1, 0},
		{"a\n  {{^a}}x{{/a}}", 2, 2},
		{"{{x}}{{#if a}}y{{else}}z{{/if}}", 1, 5},
		{"{{#if a}}y{{else if b}}z{{/if}}", 1, 0},
		{"{{{{raw}}}} {{x}} {{{{/raw}}}}", 1, 0},
		{"{{#> p}}x{{/p}}", 1, 0},
		{`{{#*inline "n"}}{{x}}{{/inline}}`, 1, 0},
		{"{{~#each a~}}{{/each}}", 1, 0},
		{"ё{{> p}}{{#with a}}{{#if b}}{{/if}}{{/with}}", 1, 8}, // first block; JS column in UTF-16 units
		{"{{#if a}}{{/if}}{{!-- never closed", 1, 0},           // parsing stops at the block
	}
	for _, c := range rejected {
		_, err := ParseWithOptions(c.src, noBlocks)
		var he *Error
		if !errors.Is(err, ErrBlocksNotAllowed) || !errors.As(err, &he) || he.Line != c.line || he.Column != c.col {
			t.Errorf("%q: got %v, want ErrBlocksNotAllowed at %d:%d", c.src, err, c.line, c.col)
		}
		if _, err := Parse(c.src); errors.Is(err, ErrBlocksNotAllowed) {
			t.Errorf("%q without NoBlocks: %v", c.src, err)
		}
	}
	if _, err := ParseWithOptions("{{#if a}}{{/if}}", noBlocks); err == nil || err.Error() != "Block statements are not allowed - 1:0" {
		t.Errorf("error text: %v", err)
	}

	// RegisterPartial parses with the template's Options.
	tpl, err := ParseWithOptions("{{> p}}", noBlocks)
	if err != nil {
		t.Fatal(err)
	}
	if err := tpl.RegisterPartial("p", "{{#if a}}{{/if}}"); !errors.Is(err, ErrBlocksNotAllowed) {
		t.Errorf("RegisterPartial: got %v, want ErrBlocksNotAllowed", err)
	}

	allowed := []string{
		"{{x}} {{{x}}} {{&x}}",
		"{{fmt x y k=x}}",
		"{{fmt (fmt x) k=(fmt y)}}",
		`{{> p}} {{> p y}} {{> p k=x}} {{> (lookup . "pn")}}`,
		"a {{! c }} {{!-- c --}} b",
		"{{* deco}}{{x}}",
		`\{{x}} {{x}}`,
	}
	data := map[string]any{"x": "<b>", "y": 2, "pn": "p"}
	for _, src := range allowed {
		var outs [2]string
		for i, opts := range []Options{{}, noBlocks} {
			tpl, err := ParseWithOptions(src, opts)
			if err != nil {
				t.Fatalf("%q (NoBlocks %v): %v", src, opts.NoBlocks, err)
			}
			tpl.RegisterHelper("fmt", func(o *HelperOptions) (any, error) { return fmt.Sprint(o.Args, o.Hash), nil })
			tpl.registerDecorator("deco", func(map[string]any, *HelperOptions) error { return nil })
			if err := tpl.RegisterPartial("p", "[{{this}}{{k}}]"); err != nil {
				t.Fatal(err)
			}
			if outs[i], err = tpl.Render(data); err != nil {
				t.Fatalf("%q (NoBlocks %v): %v", src, opts.NoBlocks, err)
			}
		}
		if outs[0] != outs[1] {
			t.Errorf("%q: NoBlocks renders %q, without it %q", src, outs[1], outs[0])
		}
	}

	// Other errors have no cause.
	_, syntaxErr := Parse("{{foo}")
	strict, err := ParseWithOptions("{{a}}", Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, renderErr := strict.Render(map[string]any{})
	for _, err := range []error{syntaxErr, renderErr} {
		var he *Error
		if !errors.As(err, &he) || errors.Unwrap(err) != nil || errors.Is(err, ErrBlocksNotAllowed) {
			t.Errorf("%v: want an *Error with no cause", err)
		}
	}
}
