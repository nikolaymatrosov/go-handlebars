package handlebars

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Recursive and exponential partials must fail with a LimitError, fast.
func TestPartialBombs(t *testing.T) {
	cases := []struct {
		name, src string
		partials  map[string]string
		what      string
	}{
		{"mutual recursion", "{{> a}}", map[string]string{"a": "{{> b}}", "b": "{{> a}}"}, "render depth"},
		{"partial block recursion", "{{#> p}}x{{/p}}", map[string]string{"p": "{{#> p}}{{> @partial-block}}{{/p}}"}, "render depth"},
		{"inline recursion", `{{#*inline "r"}}{{> r}}{{/inline}}{{> r}}`, nil, "render depth"},
		{"fan-out", "{{> p0}}", fanOut(40), "ops"},
	}
	for _, c := range cases {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatal(err)
		}
		for name, src := range c.partials {
			if err := tpl.RegisterPartial(name, src); err != nil {
				t.Fatal(err)
			}
		}
		_, err = tpl.Render(nil)
		var le *LimitError
		if !errors.As(err, &le) || le.What != c.what {
			t.Errorf("%s: err = %v, want %s LimitError", c.name, err, c.what)
		}
	}
}

// fanOut is p0..p(n-1), each calling the next twice: 2^n expansions.
func fanOut(n int) map[string]string {
	m := map[string]string{}
	for i := 0; i < n; i++ {
		next := "{{> p" + itoa(i+1) + "}}"
		m["p"+itoa(i)] = next + next
	}
	m["p"+itoa(n)] = "x"
	return m
}

// Inline decorators, binding their partials on each run and the inline
// partials scanned per call all cost ops, so a template full of them stays
// within MaxOps.
func TestInlinePartialOps(t *testing.T) {
	inlines := strings.Repeat(`{{#*inline "i"}}x{{/inline}}`, 2000)
	items := make([]any, 10000)
	for _, src := range []string{
		"{{#each items}}" + inlines + "{{/each}}",                                           // each iteration binds 2000 inline partials
		`{{#*inline "first"}}{{/inline}}` + inlines + "{{#each items}}{{> first}}{{/each}}", // scan past 2000 per call
	} {
		tpl, err := ParseWithOptions(src, Options{Limits: Limits{MaxOps: 100_000}})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = tpl.Render(map[string]any{"items": items})
		var le *LimitError
		if !errors.As(err, &le) || le.What != "ops" {
			t.Errorf("err = %v, want ops LimitError", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("took %v", d)
		}
	}
}

// A helper deleting a typed map's key while {{#each}} walks it: the
// iteration sees undefined, as JS context[field] does, instead of panicking.
func TestEachMapMutation(t *testing.T) {
	m := map[string]int{"a": 1, "b": 2}
	tpl, _ := Parse("{{#each m}}{{del}}{{@key}}={{this}};{{/each}}")
	tpl.RegisterHelper("del", func(*HelperOptions) (any, error) { delete(m, "b"); return nil, nil })
	if got, err := tpl.Render(map[string]any{"m": m}); got != "a=1;b=;" || err != nil {
		t.Errorf("got %q, %v; want %q", got, err, "a=1;b=;")
	}
}

// A hash partial copies its context's keys (JS Utils.extend); each copied
// key is an op, so a big context can't make every call cost O(len).
func TestPartialHashCopyOps(t *testing.T) {
	tpl, _ := ParseWithOptions("{{#each a}}{{> p ../a k=1}}{{/each}}", Options{Limits: Limits{MaxOps: 100_000}})
	if err := tpl.RegisterPartial("p", "{{k}}"); err != nil {
		t.Fatal(err)
	}
	_, err := tpl.Render(map[string]any{"a": make([]any, 1000)}) // 10^6 copies
	var le *LimitError
	if !errors.As(err, &le) || le.What != "ops" {
		t.Errorf("err = %v, want ops LimitError", err)
	}
}

// A LimitError inside a helper's Fn() fails the whole render, whether the
// helper returns it (wrapped) or drops it.
func TestLimitErrorsNotSwallowed(t *testing.T) {
	nest := "{{#if t}}{{#if t}}{{#if t}}x{{/if}}{{/if}}{{/if}}"
	cases := []struct {
		limits Limits
		body   string
		what   string
	}{
		{Limits{MaxOps: 2}, "{{s}}{{s}}", "ops"},
		{Limits{MaxOutputSize: 3}, "{{s}}", "output size"},
		{Limits{MaxRenderDepth: 3}, nest, "render depth"},
	}
	helpers := map[string]Helper{
		"wrap":    func(o *HelperOptions) (any, error) { _, err := o.Fn(); return nil, fmt.Errorf("wrap: %w", err) },
		"swallow": func(o *HelperOptions) (any, error) { o.Fn(); return "", nil },
	}
	for _, c := range cases {
		for name, h := range helpers {
			tpl, err := ParseWithOptions("{{#"+name+"}}"+c.body+"{{/"+name+"}}after", Options{Limits: c.limits})
			if err != nil {
				t.Fatal(err)
			}
			tpl.RegisterHelper(name, h)
			got, err := tpl.Render(map[string]any{"s": "abcdef", "t": true})
			var le *LimitError
			if !errors.As(err, &le) || le.What != c.what || got != "" {
				t.Errorf("%s/%s: %q, %v; want %s LimitError", name, c.what, got, err, c.what)
			}
		}
	}
}
