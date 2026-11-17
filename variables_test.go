package handlebars

import (
	"slices"
	"testing"
)

func TestVariables(t *testing.T) {
	cases := []struct {
		src     string
		helpers []string        // registered before Variables is called
		known   map[string]bool // Options.KnownHelpers
		want    [][]string
	}{
		// The requirements table.
		{src: "Hi {{name}}", want: [][]string{{"name"}}},
		{src: "{{a.b}} {{a.b}} {{c}}", want: [][]string{{"a", "b"}, {"c"}}},
		{src: "{{this.x}} {{./y}} {{this}} {{.}}", want: [][]string{{"x"}, {"y"}}},
		{src: "{{a.[b c].[0]}}", want: [][]string{{"a", "b c", "0"}}},
		{src: `{{lookup user "name"}}`, want: [][]string{{"user"}}},
		{src: "{{fmt price currency=cur}}", want: [][]string{{"price"}, {"cur"}}},
		{src: "{{shout}}", want: [][]string{{"shout"}}},
		{src: "{{shout}}", helpers: []string{"shout"}, want: nil},
		{src: "{{log msg}}", want: [][]string{{"msg"}}},
		{src: "{{x (pick kind)}}", want: [][]string{{"kind"}}},
		{src: "{{@index}} {{@root.a.b}}", want: [][]string{{"a", "b"}}},
		{src: "{{#if ok}}{{a}}{{else if more}}{{b}}{{else}}{{c}}{{/if}}", want: [][]string{{"ok"}, {"a"}, {"more"}, {"b"}, {"c"}}},
		{src: "{{#if ok}}{{a}}{{/if}}", helpers: []string{"if"}, want: [][]string{{"ok"}}},
		{src: "{{#each items}}{{name}} {{../title}} {{@root.site}}{{/each}}", want: [][]string{{"items"}, {"site"}}},
		{src: "{{#with user as |u|}}{{u.name}}{{/with}}", want: [][]string{{"user"}}},
		{src: "{{#items}}{{name}}{{/items}}", want: [][]string{{"items"}}},
		{src: "{{> card user k=v}}", want: [][]string{{"user"}, {"v"}}},
		{src: "{{> (pick kind)}}", want: [][]string{{"kind"}}},
		{src: `{{#*inline "p"}}{{z}}{{/inline}}{{> p}}`, want: nil},
		{src: "{{#> layout}}{{body}}{{/layout}}", want: nil},

		// The v0.2.0 decisions in ROADMAP.md.
		{src: "{{this.fmt price}}", want: [][]string{{"fmt"}, {"price"}}}, // 3: scoped callee
		{src: "{{a.b c}}", want: [][]string{{"a", "b"}, {"c"}}},           // 3: dotted callee
		{src: "{{* deco x @root.r}}", want: [][]string{{"r"}}},            // 4
		{src: `{{#*inline "p"}}{{@root.z}}{{/inline}}`, want: [][]string{{"z"}}},
		{src: "{{@root.f x}}", want: [][]string{{"f"}, {"x"}}}, // 5
		{src: "{{k}}", known: map[string]bool{"k": true}, want: nil},
		{src: "{{k}}", known: map[string]bool{"k": false}, want: [][]string{{"k"}}},          // 6
		{src: "{{if}}", known: map[string]bool{"if": false}, want: nil},                      // 6: still a builtin
		{src: "{{^if ok}}{{a}}{{else}}{{b}}{{/if}}", want: [][]string{{"ok"}, {"a"}, {"b"}}}, // 7
		{src: "{{#unless no}}{{a}}{{/unless}}", want: [][]string{{"no"}, {"a"}}},
		{src: "{{#if a}}{{#each xs}}{{y}}{{/each}}{{/if}}", want: [][]string{{"a"}, {"xs"}}},
		{src: "{{#with u}}{{else if b}}{{c}}{{/with}}", want: [][]string{{"u"}}}, // 8
		{src: "{{#if ok as |b|}}{{b}}{{/if}}", want: [][]string{{"ok"}}},
		{src: "{{a.[b.c]}} {{a.b.c}}", want: [][]string{{"a", "b.c"}, {"a", "b", "c"}}},
		{src: `{{#> layout user t=title}}{{body}}{{/layout}} {{@root}} {{"lit"}} {{> p}}`, want: [][]string{{"user"}, {"title"}}},
		{src: "{{{{raw}}}} {{x}} {{{{/raw}}}}", want: [][]string{{"raw"}}},
		{src: "text {{! c }}", want: nil},
	}
	for _, c := range cases {
		tpl, err := ParseWithOptions(c.src, Options{KnownHelpers: c.known})
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		for _, h := range c.helpers {
			tpl.RegisterHelper(h, func(*HelperOptions) (any, error) { return nil, nil })
		}
		if got := tpl.Variables(); !slices.EqualFunc(got, c.want, slices.Equal[[]string]) {
			t.Errorf("%q: Variables() = %q, want %q", c.src, got, c.want)
		}
	}

	// Each inner slice is a fresh copy.
	tpl, err := Parse("{{a.b}} {{@root.c.d}}")
	if err != nil {
		t.Fatal(err)
	}
	got := tpl.Variables()
	got[0][0], got[1][0] = "x", "x"
	if again := tpl.Variables(); again[0][0] != "a" || again[1][0] != "c" {
		t.Errorf("Variables shares its slices with the template: %q", again)
	}
}
