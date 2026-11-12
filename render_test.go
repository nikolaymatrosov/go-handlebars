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
	}
	data := map[string]any{"s": "abc", "t": true}
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
