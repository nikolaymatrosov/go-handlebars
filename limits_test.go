package handlebars

import (
	"errors"
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
