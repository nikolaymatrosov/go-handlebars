package handlebars

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func mustRender(t *testing.T, opts Options, src string, data any) string {
	t.Helper()
	tpl, err := ParseWithOptions(src, opts)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	got, err := tpl.Render(data)
	if err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return got
}

func TestRenderEscaping(t *testing.T) {
	data := map[string]any{"s": "a&b<c>d\"e'f`g=h", "safe": SafeString("<b>")}
	cases := []struct {
		opts      Options
		src, want string
	}{
		{Options{}, "{{s}}", "a&amp;b&lt;c&gt;d&quot;e&#x27;f&#x60;g&#x3D;h"},
		{Options{}, "{{{s}}}|{{&s}}", "a&b<c>d\"e'f`g=h|a&b<c>d\"e'f`g=h"},
		{Options{}, "{{safe}}", "<b>"},
		{Options{NoEscape: true}, "{{s}}", "a&b<c>d\"e'f`g=h"},
	}
	for _, c := range cases {
		if got := mustRender(t, c.opts, c.src, data); got != c.want {
			t.Errorf("%s (noEscape=%v) = %q, want %q", c.src, c.opts.NoEscape, got, c.want)
		}
	}
}

func TestAppendJSNumber(t *testing.T) {
	tenth, fifth := 0.1, 0.2 // variables: Go folds constant 0.1+0.2 to exactly 0.3
	cases := []struct {
		f    float64
		want string
	}{
		{1e21, "1e+21"}, {1e20, "100000000000000000000"}, {1e-7, "1e-7"}, {1e-6, "0.000001"},
		{math.Copysign(0, -1), "0"}, {math.NaN(), "NaN"}, {math.Inf(-1), "-Infinity"},
		{tenth + fifth, "0.30000000000000004"}, {-12.5, "-12.5"}, {1.5e300, "1.5e+300"}, {123e-20, "1.23e-18"},
	}
	for _, c := range cases {
		if got := string(appendJSNumber(nil, c.f)); got != c.want {
			t.Errorf("appendJSNumber(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestRenderLookupGoValues(t *testing.T) {
	type inner struct{ Name string }
	type outer struct {
		*inner // nil embedded pointer: promoted Name must not panic
		Items  []any
		Ptr    *inner
		hidden string
	}
	cases := []struct {
		src  string
		data any
		want string
	}{
		{"{{Ptr.Name}}|{{Name}}|{{hidden}}|{{Items.[1]}}|{{Items.length}}", outer{Ptr: &inner{"p"}, Items: []any{"a", 2.5}, hidden: "x"}, "p|||2.5|2"},
		{"{{a.b}}|{{a.missing}}|{{n}}|{{n.[1]}}", map[string]any{"a": map[string]any{"b": "x"}, "n": map[int]string{1: "no"}}, "x||[object Object]|"},
		{"{{0}}{{1}}|{{length}}", []any{"x", true}, "xtrue|2"},
		{"[{{Name}}]", (*inner)(nil), "[]"},
		// typed-nil KeyedCollection: Get must not run on a nil receiver
		{"[{{meta.title}}{{#meta}}x{{/meta}}]", map[string]any{"meta": (*omap)(nil)}, "[]"},
		{"[{{title}}]", (*omap)(nil), "[]"},
	}
	for _, c := range cases {
		if got := mustRender(t, Options{}, c.src, c.data); got != c.want {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestRenderLimits(t *testing.T) {
	cases := []struct {
		limits Limits
		src    string
		what   string
	}{
		{Limits{MaxOutputSize: 5}, "{{s}}{{s}}", "output size"},
		{Limits{MaxOps: 3}, "{{s}}{{s}}{{s}}{{s}}", "ops"},
		{Limits{MaxRenderDepth: 3}, strings.Repeat("{{#t}}", 4) + strings.Repeat("{{/t}}", 4), "render depth"},
		// array stringification is budget-bounded: 2^64 (cycle) / 2^24 (DAG) joins
		{Limits{MaxOutputSize: 100}, "{{cycle}}", "output size"},
		{Limits{MaxOutputSize: 100}, "{{{dag}}}", "output size"},
	}
	cycle := []any{nil, nil}
	cycle[0], cycle[1] = cycle, cycle
	dag := []any{"x"}
	for i := 0; i < 24; i++ {
		dag = []any{dag, dag}
	}
	data := map[string]any{"s": "abc", "t": true, "cycle": cycle, "dag": dag}
	for _, c := range cases {
		tpl, err := ParseWithOptions(c.src, Options{Limits: c.limits})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tpl.Render(data)
		var le *LimitError
		if !errors.As(err, &le) || le.What != c.what {
			t.Errorf("%s: got %v, want %s LimitError", c.what, err, c.what)
		}
	}
	tpl, _ := Parse("{{s}}")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tpl.RenderCtx(ctx, &strings.Builder{}, data); !errors.Is(err, context.Canceled) {
		t.Errorf("RenderCtx on a canceled ctx = %v, want context.Canceled", err)
	}
}

// TestRenderBlockParams drives the internal block-param path: helper "bp"
// passes (ctx, [1, {c: "C"}]) to its block, the way builtins will.
func TestRenderBlockParams(t *testing.T) {
	bp := func(o *HelperOptions) (any, error) {
		return nil, o.s.renderProgramBP(o.fn, o.Ctx(), bpFrame{vals: []any{"A", map[string]any{"c": "C"}}})
	}
	cases := []struct{ src, want string }{
		{"{{#bp as |a b|}}{{a}}{{b.c}}{{/bp}}", "AC"},
		{"{{#bp as |a b|}}{{#bp as |x|}}{{a}}{{x}}{{b.c}}{{/bp}}{{/bp}}", "AAC"},
		{"{{#bp as |a|}}{{#with2}}{{a}}{{/with2}}{{/bp}}", "A"}, // plain block between: depth counts it
		{"{{#bp as |a|}}{{#bp as |a|}}{{a}}{{/bp}}{{/bp}}", "A"},
		{"{{#bp as |a|}}{{/bp}}{{a}}", "ctx"}, // out of scope
		{"{{#bp as |a|}}{{this.a}}{{/bp}}", "ctx"},
		{`{{#bp as |a|}}{{"a"}}{{/bp}}`, "A"}, // transformLiteralToPath, then a block param
		{"{{#bp as |a b c|}}[{{c}}]{{/bp}}", "[]"},
		// chained else: the wrapper inverse program is a frame; o is 2 levels out
		// ('a' is declared on the sibling program, so it is a context lookup).
		{"{{#bp as |o|}}{{#bpi as |a|}}x{{else bp as |b|}}{{o}}{{b}}{{a}}{{/bpi}}{{/bp}}", "AActx"},
		{"{{#bp as |o|}}{{#bpi as |a|}}x{{else bp as |b|}}{{#with2}}{{o}}{{b}}{{/with2}}{{/bpi}}{{/bp}}", "AA"},
	}
	for _, c := range cases {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatal(c.src, err)
		}
		tpl.RegisterHelper("bp", bp)
		tpl.RegisterHelper("bpi", func(o *HelperOptions) (any, error) { return nil, o.s.renderProgram(o.inverse, o.Ctx()) })
		tpl.RegisterHelper("with2", func(o *HelperOptions) (any, error) { return nil, o.s.renderProgram(o.fn, o.Ctx()) })
		if got, err := tpl.Render(map[string]any{"a": "ctx"}); err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.src, got, err, c.want)
		}
	}
}

func TestRenderIsEmpty(t *testing.T) {
	for v, want := range map[string]bool{"nil": true, "0": false, "empty": true, "arr0": true, "arr1": false, "map": false, "str": true, "x": false} {
		var in any
		switch v {
		case "0":
			in = 0.0
		case "empty", "str":
			in = ""
		case "arr0":
			in = []any{}
		case "arr1":
			in = []any{1}
		case "map":
			in = map[string]any{}
		case "x":
			in = "x"
		}
		if got := isEmpty(in); got != want {
			t.Errorf("isEmpty(%s) = %v", v, got)
		}
	}
	if !isEmpty(math.NaN()) || isEmpty(int8(0)) || !isEmpty(([]int)(nil)) {
		t.Error("isEmpty NaN/int/nil slice")
	}
}

func TestRenderCompatDepthed(t *testing.T) {
	got := mustRender(t, Options{Compat: true}, "{{#o}}{{#i}}{{x.y}}{{this.x.y}}|{{/i}}{{/o}}", map[string]any{
		"x": map[string]any{"y": "Y"}, "o": []any{map[string]any{"i": []any{1}}}})
	if got != "Y|" {
		t.Errorf("got %q", got)
	}
}

// TestHelperAPI covers HelperOptions and dispatch behavior the spec corpus
// doesn't: writer entry points, recovery, sticky limits, compile errors.
func TestHelperAPI(t *testing.T) {
	echo := func(o *HelperOptions) (any, error) { return o.Name + ":" + jsStr(arg(o, 0)), nil }
	helpers := map[string]Helper{
		"bp": func(o *HelperOptions) (any, error) {
			var sb strings.Builder
			err := o.FnTo(&sb, o.Ctx(), []any{map[string]any{"fn": Helper(echo)}}, nil)
			return SafeString(sb.String()), err
		},
		"inv": func(o *HelperOptions) (any, error) {
			var sb strings.Builder
			err := o.InverseTo(&sb, "X")
			return sb.String(), err
		},
		"names":   func(o *HelperOptions) (any, error) { return strings.Join(o.BlockParamNames(), ","), nil },
		"look":    func(o *HelperOptions) (any, error) { return o.Lookup("a.b"), nil },
		"swallow": func(o *HelperOptions) (any, error) { o.Fn(); return "", nil }, // drops the LimitError
		"spin": func(o *HelperOptions) (any, error) { // a helper looping over an empty block
			for {
				if _, err := o.Fn(); err != nil {
					return nil, err
				}
			}
		},
		"boom":               func(o *HelperOptions) (any, error) { panic("boom") },
		"blockHelperMissing": func(o *HelperOptions) (any, error) { return "hook", nil },
	}
	data := map[string]any{"a": map[string]any{"b": "B"}, "s": "abcdef"}
	cases := []struct {
		opts      Options
		src, want string // want "!x": an error containing x
	}{
		{Options{}, "{{#bp as |b|}}{{b.fn 1}}{{/bp}}", "b.fn:1"}, // multi-part block param path is a helper call
		{Options{}, "{{#inv}}x{{else}}{{this}}{{/inv}}", "X"},
		{Options{}, "{{#names as |a b|}}{{/names}}|{{names}}", "a,b|"},
		{Options{}, "{{look}}", "B"},
		{Options{}, "{{#a}}{{/a}}", "hook"},
		{Options{}, "{{blockHelperMissing 1}}", `!Missing helper: "blockHelperMissing"`}, // hooks aren't helpers
		{Options{Limits: Limits{MaxOutputSize: 3}}, "{{#swallow}}{{s}}{{/swallow}}", "!output size"},
		{Options{Limits: Limits{MaxOps: 1}}, "{{#swallow}}{{s}}{{/swallow}}", "!ops"}, // no later step to trip
		{Options{Limits: Limits{MaxOps: 100}}, "{{#spin}}{{/spin}}", "!ops"},
		{Options{}, "{{boom}}", "!handlebars: helper panicked (boom): boom\ngoroutine "}, // with the stack
		{Options{}, "{{#bp as |b|}}{{#look}}{{/look}}{{boom}}{{/bp}}", "!helper panicked (boom)"},
	}
	for _, c := range cases {
		tpl, err := ParseWithOptions(c.src, c.opts)
		if err != nil {
			t.Fatal(c.src, err)
		}
		for name, h := range helpers {
			tpl.RegisterHelper(name, h)
		}
		got, err := tpl.Render(data)
		if err != nil {
			got = "!" + err.Error()
		}
		if got != c.want && !(c.want[0] == '!' && strings.Contains(got, c.want[1:])) {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}

	tpl, _ := Parse("{{boom}}")
	tpl.RegisterHelper("boom", helpers["boom"])
	if _, err := tpl.Render(nil); !errors.Is(err, ErrHelperPanic) {
		t.Errorf("helper panic: %v, want ErrHelperPanic", err)
	}
	// Library code is never recovered: a panic below a plain section (here a
	// KeyedCollection's Get) must reach the caller, not pose as a helper panic.
	func() {
		defer func() {
			if r := recover(); r != "library" {
				t.Errorf("plain section recovered a library panic: %v", r)
			}
		}()
		tpl, _ := Parse("{{#x}}{{y}}{{/x}}")
		tpl.Render(map[string]any{"x": panicKC{}})
	}()

	// knownHelpersOnly rejects unknown helpers at compile time, even in
	// branches that never run; a parse error still wins (JS parses first).
	ko := Options{KnownHelpersOnly: true}
	if _, err := ParseWithOptions("{{#if false}}{{foo 1}}{{/if}}", ko); err == nil || err.Error() != "You specified knownHelpersOnly, but used the unknown helper foo - 1:13" {
		t.Errorf("knownHelpersOnly: %v", err)
	}
	if _, err := ParseWithOptions("{{foo 1}}{{", ko); err == nil || !strings.HasPrefix(err.Error(), "Parse error") {
		t.Errorf("knownHelpersOnly + parse error: %v", err)
	}
}

// panicKC is a KeyedCollection whose Get panics.
type panicKC struct{}

func (panicKC) Keys() []string         { return nil }
func (panicKC) Get(string) (any, bool) { panic("library") }
func (panicKC) Len() int               { return 0 }

func TestBuiltinsExtra(t *testing.T) {
	type S struct {
		A int
		B string
	}
	for _, c := range []struct {
		src  string
		data any
		want string
	}{
		{"{{#each s}}{{@key}}={{this}}{{#unless @last}},{{/unless}}{{/each}}", map[string]any{"s": S{1, "x"}}, "A=1,B=x"},
		{"{{#each m}}{{@key}}{{/each}}", map[string]any{"m": map[string]any{"b": 1, "a": 2}}, "ab"},
		{"{{#each s}}x{{else}}none{{/each}}", map[string]any{"s": "str"}, "none"},
		{"{{#if z includeZero=true}}y{{else}}n{{/if}}{{#if z}}y{{else}}n{{/if}}", map[string]any{"z": 0}, "yn"},
		{"{{#each a}}{{#each ../a}}{{@../index}}{{@index}} {{/each}}{{/each}}", map[string]any{"a": []any{1, 2}}, "00 01 10 11 "},
		{"{{#people}}{{@index}}{{/people}}", map[string]any{"people": []any{1, 2}}, "01"},
		{"{{lookup a 1}}{{lookup n 'k'}}", map[string]any{"a": []any{"p", "q"}}, "q"},
	} {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := tpl.Render(c.data); err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.src, got, err, c.want)
		}
	}
	for src, want := range map[string]string{
		"{{#if}}x{{/if}}": "#if requires exactly one argument", "{{#unless a b}}x{{/unless}}": "#unless requires exactly one argument",
		"{{#with}}x{{/with}}": "#with requires exactly one argument", "{{#each}}x{{/each}}": "Must pass iterator to #each",
	} {
		tpl, _ := Parse(src)
		if _, err := tpl.Render(nil); err == nil || err.Error() != want {
			t.Errorf("%s: %v", src, err)
		}
	}
	var got []string
	tpl, _ := Parse("{{log 'a' 'b'}}{{log 'w' level='warn'}}{{log 'd' level='debug'}}{{log 'n' level=7}}")
	tpl.Logger = func(level string, args ...any) { got = append(got, level+":"+jsStr(args[0])) }
	if _, err := tpl.Render(nil); err != nil || strings.Join(got, " ") != "info:a warn:w log:n" {
		t.Errorf("log: %v %v", got, err)
	}
}

func TestLoggerPanicRecovered(t *testing.T) {
	tpl, _ := Parse("a{{log 'x'}}b")
	tpl.Logger = func(string, ...any) { panic("boom") }
	if _, err := tpl.Render(nil); !errors.Is(err, ErrHelperPanic) {
		t.Errorf("err = %v, want ErrHelperPanic", err)
	}
}

func TestDataRootAndFrames(t *testing.T) {
	for _, c := range []struct {
		src  string
		data any
		vars map[string]any
		want string
	}{
		{"{{@root.a}}", map[string]any{"a": "x"}, nil, "x"},
		{"{{@root.a}}", map[string]any{"a": "x"}, map[string]any{"root": map[string]any{"a": "y"}}, "y"},
		{"{{@../v}}", nil, map[string]any{"v": 1}, "1"},                               // root frame is a child of the seeded vars
		{"{{#as_ x as |foo|}}{{@foo}}{{/as_}}", nil, map[string]any{"foo": "d"}, "d"}, // block params never shadow @foo
		// a non-object root
		{rootSrc, "abc", nil, "abc|3||w|abc"},
		{rootSrc, []any{"x", "y"}, nil, "x,y|2|22|w|x,y"},
		{rootSrc, 42.0, nil, "42|||w|42"},
		{rootSrc, false, nil, "false||||false"},
		{rootSrc, nil, nil, "||||"},
		// each -> FnTo(dataVars) -> each: the helper's frame sits between the
		// two loop frames and inherits the outer one's variables (createFrame)
		{"{{#each outer}}{{#fr}}{{#each @root.inner}}{{@index}}{{@../x}}{{@../index}}{{@../../index}}[{{@../../../index}}],{{/each}}{{/fr}}{{/each}}",
			map[string]any{"outer": []any{"a", "b"}, "inner": []any{1, 2}}, nil, "0F00[],1F00[],0F11[],1F11[],"},
	} {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatal(err)
		}
		registerDataHelpers(tpl)
		var sb strings.Builder
		if err := tpl.RenderWithData(&sb, c.data, c.vars); err != nil || sb.String() != c.want {
			t.Errorf("%s = %q, %v; want %q", c.src, sb.String(), err, c.want)
		}
	}

	// Data off: no frames at all, so FnTo's dataVars, DataVar, seeded vars
	// and loop variables are all undefined.
	off := false
	tpl, _ := ParseWithOptions("{{#fr}}[{{@x}}|{{peek}}|{{#each a}}{{@index}}{{@../x}}{{/each}}|{{@root}}{{@v}}]{{/fr}}", Options{Data: &off})
	registerDataHelpers(tpl)
	var sb strings.Builder
	if err := tpl.RenderWithData(&sb, map[string]any{"a": []any{1, 2}}, map[string]any{"v": 1}); err != nil || sb.String() != "[|||]" {
		t.Errorf("data off = %q, %v; want %q", sb.String(), err, "[|||]")
	}
}

const rootSrc = "{{@root}}|{{@root.length}}|{{#each @root}}{{@root.length}}{{/each}}|{{#with @root}}w{{/with}}|{{> p}}"

func registerDataHelpers(tpl *Template) {
	tpl.RegisterHelper("as_", func(o *HelperOptions) (any, error) { return o.Fn() })
	tpl.RegisterHelper("fr", func(o *HelperOptions) (any, error) {
		var sb strings.Builder
		err := o.FnTo(&sb, o.Ctx(), nil, map[string]any{"x": "F"})
		return SafeString(sb.String()), err
	})
	tpl.RegisterHelper("peek", func(o *HelperOptions) (any, error) { return o.DataVar("x"), nil })
	tpl.RegisterPartial("p", "{{@root}}")
}

func TestPartialsExtra(t *testing.T) {
	type user struct{ Name string }
	raw, _ := ParseWithOptions("{{h}}", Options{NoEscape: true}) // renders with its own options
	cases := []struct {
		src, want string
		partials  map[string]string
	}{
		// JS indents the partial's whole result, newlines from data included.
		{"x\n  {{> p}}\ny", "x\n  a\n  b|\n  a\n  by", map[string]string{"p": "{{v}}|\n{{v}}"}},
		// A helper's Fn() buffer is indented once, when the helper's result is written.
		{"  {{> p}}", "  1\n  2", map[string]string{"p": "{{#wrap}}1\n2{{/wrap}}"}},
		{"{{h}}{{> raw}}", "&lt;b&gt;<b>", nil},
		{"{{> p u greeting='hi'}}", "hi Ann", map[string]string{"p": "{{greeting}} {{Name}}"}},
	}
	for _, c := range cases {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatal(err)
		}
		tpl.RegisterHelper("wrap", func(o *HelperOptions) (any, error) { return o.Fn() })
		tpl.RegisterPartialTemplate("raw", raw)
		for name, src := range c.partials {
			if err := tpl.RegisterPartial(name, src); err != nil {
				t.Fatal(err)
			}
		}
		got, err := tpl.Render(map[string]any{"v": "a\nb", "h": "<b>", "u": user{"Ann"}})
		if err != nil || got != c.want {
			t.Errorf("%q = %q, %v; want %q", c.src, got, err, c.want)
		}
	}
}

func TestPartialScopes(t *testing.T) {
	// A saved @partial-block replays with the scope it was created in, not
	// with stack slots or {{#each}} state reused since (JS: "<a>").
	tpl, _ := Parse(`{{#each items as |x|}}{{#> p}}<{{x}}>{{/p}}{{/each}}|{{#replay}}{{> @partial-block}}{{/replay}}`)
	var saved any
	tpl.RegisterHelper("save", func(o *HelperOptions) (any, error) {
		if saved == nil {
			saved = o.DataVar("partial-block")
		}
		return nil, nil
	})
	tpl.RegisterHelper("replay", func(o *HelperOptions) (any, error) {
		var sb strings.Builder
		err := o.FnTo(&sb, o.Ctx(), nil, map[string]any{"partial-block": saved})
		return sb.String(), err
	})
	if err := tpl.RegisterPartial("p", "{{save}}{{> @partial-block}}"); err != nil {
		t.Fatal(err)
	}
	if got, err := tpl.Render(map[string]any{"items": []any{"a", "b", "c"}}); got != "<a><b><c>|<a>" || err != nil {
		t.Errorf("replay = %q, %v; want %q", got, err, "<a><b><c>|<a>")
	}

	// JS decorators get the depths outside their program, so an inline
	// partial's ../ skips the defining context, like a registered partial's.
	data := map[string]any{"title": "T", "items": []any{map[string]any{"name": "a"}}}
	for _, src := range []string{
		`{{#*inline "row"}}{{name}} of {{../title}};{{/inline}}{{#each items}}{{> row}}{{/each}}`,
		`{{#each items}}{{> row}}{{/each}}`,
	} {
		tpl, _ := Parse(src)
		if err := tpl.RegisterPartial("row", "{{name}} of {{../title}};"); err != nil {
			t.Fatal(err)
		}
		if got, err := tpl.Render(data); got != "a of ;" || err != nil {
			t.Errorf("%s = %q, %v; want %q", src, got, err, "a of ;")
		}
	}
}

// Decorators run when JS wraps their program: once per block statement or
// partial block call, not per {{#each}} iteration, and not again when the
// partial block renders.
func TestDecoratorTiming(t *testing.T) {
	cases := []struct {
		src, want string
		runs      int
	}{
		{"{{#each items}}{{*count}}{{/each}}", "", 1},
		{"{{#each items}}{{#with .}}{{*count}}{{/with}}{{/each}}", "", 3},
		{`{{#> p}}{{#*inline (count)}}I{{/inline}}{{> x}}{{/p}}`, "II", 1}, // inline name evaluated once
	}
	for _, c := range cases {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatal(err)
		}
		runs := 0
		tpl.registerDecorator("count", func(map[string]any, *HelperOptions) error { runs++; return nil })
		tpl.RegisterHelper("count", func(*HelperOptions) (any, error) { runs++; return "x", nil })
		if err := tpl.RegisterPartial("p", "{{> x}}{{> @partial-block}}"); err != nil {
			t.Fatal(err)
		}
		got, err := tpl.Render(map[string]any{"items": []any{1, 2, 3}})
		if got != c.want || err != nil || runs != c.runs {
			t.Errorf("%s = %q, %v, %d runs; want %q, %d runs", c.src, got, err, runs, c.want, c.runs)
		}
	}

	tpl, _ := Parse("{{#if true}}{{*nope}}{{/if}}")
	if _, err := tpl.Render(nil); err == nil || err.Error() != `Missing decorator: "nope"` {
		t.Errorf("unknown decorator: err = %v", err)
	}
	tpl.registerDecorator("nope", func(map[string]any, *HelperOptions) error { panic("boom") })
	if _, err := tpl.Render(nil); !errors.Is(err, ErrHelperPanic) {
		t.Errorf("panicking decorator: err = %v, want ErrHelperPanic", err)
	}
}

func TestStrictErrorLocation(t *testing.T) {
	tpl, err := ParseWithOptions("\n\n\n   {{hello}}", Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tpl.Render(map[string]any{})
	if want := `"hello" not defined in [object Object] - 4:5`; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}

type nilKC struct{}

func (*nilKC) Keys() []string         { return nil }
func (*nilKC) Get(string) (any, bool) { return nil, false }
func (*nilKC) Len() int               { return 0 }

func TestStrictAssumeObjectsTable(t *testing.T) {
	type S struct{ B int }
	tp := func(src string, o Options) *Template {
		tpl, err := ParseWithOptions(src, o)
		if err != nil {
			t.Fatal(err)
		}
		return tpl
	}
	partial := tp("{{x}}", Options{Strict: true})
	cases := []struct {
		name  string
		tpl   *Template
		data  any
		want  string // output, or error text when isErr
		isErr bool
	}{
		{"assume nil intermediate", tp("{{a.b.c}}", Options{AssumeObjects: true}), map[string]any{"a": nil}, "Cannot read properties of undefined (reading 'b')", true},
		{"assume typed-nil pointer", tp("{{a.B}}", Options{AssumeObjects: true}), map[string]any{"a": (*S)(nil)}, "Cannot read properties of undefined (reading 'B')", true},
		{"assume typed-nil KeyedCollection", tp("{{a.b}}", Options{AssumeObjects: true}), map[string]any{"a": (*nilKC)(nil)}, "Cannot read properties of undefined (reading 'b')", true},
		{"assume ok", tp("{{a.b}}", Options{AssumeObjects: true}), map[string]any{"a": map[string]any{}}, "", false},
		{"strict missing @var", tp("{{@foo}}", Options{Strict: true}), map[string]any{}, `"foo" not defined in [object Object] - 1:2`, true},
		{"strict partial own options", func() *Template {
			t := tp("{{> p}}", Options{})
			t.RegisterPartialTemplate("p", partial)
			return t
		}(), map[string]any{}, `"x" not defined in [object Object] - 1:2`, true},
	}
	for _, c := range cases {
		got, err := c.tpl.Render(c.data)
		switch {
		case c.isErr && (err == nil || err.Error() != c.want):
			t.Errorf("%s: got %q, %v; want error %q", c.name, got, err, c.want)
		case !c.isErr && (err != nil || got != c.want):
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// Task 10c: an inline partial captured no depths, so JS depths[0] is
// undefined (not null) and nullContext is still pushed: ../. is the object.
func TestNullCtxEmptyDepths(t *testing.T) {
	tpl, err := Parse(`{{#*inline "row"}}{{#if 1}}{{#with @root.x}}[{{../.}}]{{/with}}{{/if}}{{/inline}}{{> row nope}}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tpl.Render(map[string]any{"x": map[string]any{}})
	if want := "[[object Object]]"; got != want || err != nil {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
}

// Sparse []any: holes are skipped by each, invisible to lookup, empty in join.
func TestSparseHoles(t *testing.T) {
	arr := []any{hole{}, "foo", hole{}, "bar"}
	for src, want := range map[string]string{
		`{{#each a}}{{@index}}{{.}}{{@last}}{{/each}}`: "1foofalse3bartrue",
		`{{a}}`:                          ",foo,,bar",
		`{{#each a}}{{@first}}{{/each}}`: "falsefalse",
		`{{a.length}}{{a.[0]}}{{a.[1]}}`: "4foo",
	} {
		tpl, perr := Parse(src)
		if perr != nil {
			t.Fatal(perr)
		}
		if got, err := tpl.Render(map[string]any{"a": arr}); got != want || err != nil {
			t.Errorf("%s = %q, %v; want %q", src, got, err, want)
		}
	}
}

// TestLegacyOptions pins StringParams/TrackIDs behavior the spec corpus
// doesn't reach (partials, data off, Go block params, builtins on strings).
func TestLegacyOptions(t *testing.T) {
	off := false
	track, str := Options{TrackIDs: true}, Options{StringParams: true}
	cases := []struct {
		opts      Options
		src, want string
	}{
		{track, "{{#each arr as |v i|}}{{ids v.n i}}{{/each}}", "arr.0.n,null"},
		{Options{TrackIDs: true, Data: &off}, "{{#each arr as |v i|}}{{ids v.n i}}{{/each}}", "undefined0.n,null"}, // JS contextPath stays undefined
		{track, "{{> p foo}}{{cp}}", "foo;foo"},                                                                    // invokePartial sets the caller's frame
		{track, `{{> p "x"}}{{cp}}`, ";"},                                                                          // a literal's null id keeps it
		{track, "{{#with foo}}{{#with this}}{{cp}}{{/with}}{{/with}}", "foo."},
		{track, `{{#with "lit"}}{{cp}}{{/with}}`, "null"},
		{track, "{{#bp as |x|}}{{ids x.y}}{{/bp}}", "undefined.y"}, // FnTo passes no blockParams.path
		{track, "{{#bpp as |x|}}{{ids x.y x}}{{/bpp}}", "zomg.y,zomg"},
		{track, "{{> p foo a=1}}{{cp}}", "true;true"}, // a hash makes ids[0] true (invokePartialWrapper)
		{track, "{{> p a=1}}{{cp}}", "true;true"},
		{str, "{{args ../a ./b c/d this . null undefined 1 @x}}",
			"[a b c.d this .   1 @x] [PathExpression PathExpression PathExpression PathExpression PathExpression NullLiteral UndefinedLiteral NumberLiteral PathExpression] map[]"},
		{str, `{{args "a/b" "./c" 1 true x="../d/e"}}`, "[a.b c 1 true] [StringLiteral StringLiteral NumberLiteral BooleanLiteral] map[x:d.e]"}, // strings get the path transform too
		{str, "{{> q foo}}|{{> q}}", "foo|"}, // the implicit partial context is a string param too
		{str, "{{#if nope}}y{{else}}n{{/if}}", "y"},
		{Options{StringParams: true, TrackIDs: true}, "{{ids foo}}", ""}, // stringParams wins, as in pushParam
	}
	for _, c := range cases {
		tpl, err := ParseWithOptions(c.src, c.opts)
		if err != nil {
			t.Fatal(c.src, err)
		}
		tpl.RegisterHelper("ids", func(o *HelperOptions) (any, error) {
			if o.Legacy == nil {
				return "", nil
			}
			return strings.Join(o.Legacy.IDs, ","), nil
		})
		tpl.RegisterHelper("cp", func(o *HelperOptions) (any, error) { return o.DataVar("contextPath"), nil })
		tpl.RegisterHelper("args", func(o *HelperOptions) (any, error) { return fmt.Sprint(o.Args, o.Legacy.Types, o.Hash), nil })
		tpl.RegisterHelper("bp", func(o *HelperOptions) (any, error) {
			var sb strings.Builder
			err := o.FnTo(&sb, o.Ctx(), []any{map[string]any{"y": 1}}, nil)
			return sb.String(), err
		})
		tpl.RegisterHelper("bpp", func(o *HelperOptions) (any, error) {
			var sb strings.Builder
			err := o.FnToPaths(&sb, o.Ctx(), []any{map[string]any{"y": 1}}, []string{"zomg"}, nil)
			return sb.String(), err
		})
		for name, src := range map[string]string{"p": "{{cp}};", "q": "{{this}}"} {
			if err := tpl.RegisterPartial(name, src); err != nil {
				t.Fatal(err)
			}
		}
		data := map[string]any{"arr": []any{map[string]any{"n": 1}}, "foo": map[string]any{"bar": "x"}}
		if got, err := tpl.Render(data); err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.src, got, err, c.want)
		}
	}
}
