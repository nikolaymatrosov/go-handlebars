package handlebars

import (
	"errors"
	"testing"
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
