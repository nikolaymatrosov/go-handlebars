package handlebars

import (
	"context"
	"errors"
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
		return nil, o.s.renderProgramBP(o.fn, o.Ctx(), []any{"A", map[string]any{"c": "C"}})
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
