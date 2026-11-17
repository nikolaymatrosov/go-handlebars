package handlebars

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Fuzz invariants: Parse and Render never panic (nor does Variables, which returns no empty or duplicate path) (library panics propagate
// raw, so the fuzzer sees them), stay within their limits, finish fast,
// and fail only with a template *Error or a *LimitError. No user helpers
// are registered, so an ErrHelperPanic is a library bug too.

var fuzzLimits = Limits{MaxTemplateSize: 1 << 16, MaxParseDepth: 64, MaxOutputSize: 1 << 12, MaxOps: 1 << 14}

// fuzzOptions turns fuzzed bits into compile options.
func fuzzOptions(bits uint16) Options {
	b := func(i int) bool { return bits&(1<<i) != 0 }
	o := Options{Limits: fuzzLimits, Strict: b(0), AssumeObjects: b(1), Compat: b(2), KnownHelpersOnly: b(3),
		IgnoreStandalone: b(4), PreventIndent: b(5), ExplicitPartialContext: b(6), NoEscape: b(7),
		StringParams: b(8), TrackIDs: b(9), NoBlocks: b(11)}
	if b(10) {
		o.Data = new(bool) // data: false
	}
	if b(12) {
		o.Limits.MaxExpressions = 16
	}
	return o
}

// checkErr fails unless err is nil, an *Error or a *LimitError.
func checkErr(t *testing.T, what string, err error) {
	t.Helper()
	var he *Error
	var le *LimitError
	if err != nil && !errors.As(err, &he) && !errors.As(err, &le) {
		t.Fatalf("%s: unexpected error %T: %v", what, err, err)
	}
}

// fuzzRender renders tpl and checks the invariants.
func fuzzRender(t *testing.T, tpl *Template, data any) {
	t.Helper()
	var sb strings.Builder
	start := time.Now()
	err := tpl.RenderTo(&sb, data)
	if d := time.Since(start); d > 2*time.Second { // milliseconds under fuzzLimits
		t.Fatalf("render took %v", d)
	}
	checkErr(t, "render", err)
	if sb.Len() > fuzzLimits.MaxOutputSize {
		t.Fatalf("output %d bytes > MaxOutputSize %d", sb.Len(), fuzzLimits.MaxOutputSize)
	}
}

func FuzzParse(f *testing.F) {
	for _, src := range []string{ // from the spec corpus
		"{{foo}}",
		"Goodbye {{\"foo bar\"}} world!",
		"{{#goodbyes}}{{text}}! {{/goodbyes}}cruel {{world}}!",
		"{{#goodbyes}}{{this}}{{/goodbyes}}{{^goodbyes}}Right On!{{/goodbyes}}",
		"{{#people}}\n{{name}}\n{{else if none}}\n{{none}}\n{{^}}\n{{/people}}\n",
		"{{#each goodbyes}}{{@index}}. {{text}}! {{/each}}cruel {{world}}!",
		"{{#each goodbyes as |value index|}}{{index}}. {{value.text}}!{{/each}}",
		"{{#with person as |foo|}}{{foo.first}} {{last}}{{/with}}",
		"{{#goodbyes}}{{text}} cruel {{@foo/../name}}! {{/goodbyes}}",
		"{{#goodbyes}}{{{link ../prefix}}}{{/goodbyes}}",
		"{{lookup . 'a'}} {{lookup a 1}} {{log 'x' level='warn'}}",
		"{{dash c.c (concat a (b 'x' k=true) 1.5 -2)}}",
		"Dudes: {{#dudes}}{{> dude}}{{/dudes}}",
		"<template>{{#> outer}}{{value}}{{/outer}}</template>",
		"{{#*inline \"myPartial\"}}success{{/inline}}{{#> p}}{{> myPartial}}{{/p}}",
		"{{> (lookup . 'name') this x=1}}\n  {{> p}}\n",
		"{{{{raw}}}} {{test}} {{{{/raw}}}}",
		"{{!-- a comment }} --}}{{! short }}\\{{escaped}}",
		" {{~#if foo~}} bar {{~^~}} baz {{~/if~}} ",
		"{{*decorator}}{{#if a}}{{*decorator x}}{{/if}}",
		"{{[foo bar]}}{{foo.[0]}}{{@root.a}}{{this/foo}}{{../x}}",
		" {{#each a}}\r\n{{/each}}",
	} {
		f.Add(src, uint16(0))
		f.Add(src, uint16(1<<11|1<<12)) // NoBlocks, MaxExpressions 16
	}
	f.Fuzz(func(t *testing.T, src string, bits uint16) {
		opts := fuzzOptions(bits)
		start := time.Now()
		tpl, err := ParseWithOptions(src, opts)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("parse took %v", d)
		}
		checkErr(t, "parse", err)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, p := range tpl.Variables() {
			k := fmt.Sprintf("%q", p)
			if len(p) == 0 || seen[k] {
				t.Fatalf("Variables() = %q: empty or duplicate path", tpl.Variables())
			}
			seen[k] = true
		}
		tpl.RegisterPartialTemplate("p", tpl) // self-recursion
		fuzzRender(t, tpl, map[string]any{"a": []any{1.0, "x", nil}})
	})
}

// FuzzRender fuzzes the data (JSON: insertion-ordered objects and plain Go
// maps) against fixed templates.
func FuzzRender(f *testing.F) {
	type tc struct {
		src  string
		opts Options
	}
	cases := []tc{
		{"{{#each this}}{{@key}}{{@index}}{{@first}}{{@last}}:{{.}}|{{#each .}}{{@../key}}{{this}}{{/each}}{{else}}none{{/each}}", Options{}},
		{"{{#with a as |x|}}{{x.b}}{{#if x}}{{lookup x 'c'}}{{/if}}{{else}}no{{/with}}", Options{}},
		{"{{#a}}{{#b}}{{c}}{{../d}}{{/b}}{{/a}}{{^a}}no{{/a}}", Options{}},
		{"{{lookup this a}}{{lookup a b}}{{lookup . (lookup . 'k')}}", Options{}},
		{"{{> (lookup . 'name') this x=a}}", Options{}},
		{"{{> dude a k=b}}{{> dude}}", Options{}},
		{"{{#if a}}{{#unless b}}{{a.length}}{{/unless}}{{/if}}{{#if a includeZero=true}}z{{/if}}", Options{}},
		{"{{log a b level=c}}{{log a}}", Options{}},
		{"{{a.b.c}}{{#with a}}{{b}}{{/with}}{{#each a}}{{c}}{{/each}}", Options{Strict: true}},
		{"{{a.b.c}}{{#a}}{{b.c}}{{/a}}", Options{AssumeObjects: true}},
		{"{{#a}}{{#b}}{{c}}{{d}}{{/b}}{{/a}}", Options{Compat: true}},
		{"{{#each a}}{{#with b}}{{> dude}}{{/with}}{{lookup . 'c'}}{{/each}}", Options{TrackIDs: true}},
		{"{{#each a}}{{lookup ../b c}}{{/each}}", Options{StringParams: true}},
		{"{{@root.a}}{{#each a as |v k|}}{{k}}={{v}}{{#> dude}}{{v}}{{/dude}}{{/each}}", Options{}},
		{"{{a}}{{{a}}}{{&b}}{{a.[0]}}{{a.length}}", Options{}},
		{"{{#each a}}{{#each ../a}}{{this}}{{/each}}{{/each}}", Options{}},
	}
	tpls := make([]*Template, len(cases))
	for i, c := range cases {
		c.opts.Limits = fuzzLimits
		tpl, err := ParseWithOptions(c.src, c.opts)
		if err != nil {
			f.Fatal(c.src, err)
		}
		for name, src := range map[string]string{"dude": "{{name}}{{x}}{{k}}{{> @partial-block}}", "a": "{{this}}", "b": "{{> a}}"} {
			if err := tpl.RegisterPartial(name, src); err != nil {
				f.Fatal(err)
			}
		}
		tpl.Logger = func(string, ...any) {}
		tpls[i] = tpl
	}
	for _, seed := range []string{
		`{"a":[1,"x",null],"b":{"c":"d"}}`,
		`{"a":{"b":{"c":"<&>"}},"d":true,"name":"dude"}`,
		`{"goodbyes":[{"text":"goodbye"},{"text":"Goodbye"}],"world":"world"}`,
		`{"a":[[1,[2,[3]]],{"b":[]}],"b":0,"c":"length","k":"a"}`,
		`{"a":0,"b":"","c":"warn","name":"b"}`,
		`[1,2,{"a":[3]}]`,
		`"str"`, `12.5e300`, `null`, `false`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		ordered, err := decodeOrdered(raw)
		if err != nil {
			return
		}
		var plain any
		_ = json.Unmarshal(raw, &plain)
		for _, tpl := range tpls {
			fuzzRender(t, tpl, ordered)
			fuzzRender(t, tpl, plain)
		}
	})
}
