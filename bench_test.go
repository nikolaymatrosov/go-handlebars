//go:build !race

// The race detector makes sync.Pool drop items at random, so allocation
// counts only hold without it (benchmarks under -race mean nothing anyway).

package handlebars

import (
	"io"
	"strings"
	"testing"

	"github.com/nikthespirit/go-handlebars/internal/benchdata"
)

// Run: go test -bench . -benchmem -run '^$'

// BenchmarkRender renders each scenario to a string.
func BenchmarkRender(b *testing.B) {
	for _, sc := range benchdata.Scenarios {
		t, data := compileScenario(b, sc), sc.Data()
		b.Run(sc.Name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := t.Render(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRenderTo renders each scenario to a reusable writer.
func BenchmarkRenderTo(b *testing.B) {
	for _, sc := range benchdata.Scenarios {
		t, data := compileScenario(b, sc), sc.Data()
		b.Run(sc.Name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := t.RenderTo(io.Discard, data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkParse is the compile cost: the template and its partials.
func BenchmarkParse(b *testing.B) {
	for _, sc := range benchdata.Scenarios {
		b.Run(sc.Name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				compileScenario(b, sc)
			}
		})
	}
}

// TestRenderAllocs pins the allocation-free renders, so a regression fails
// go test, not just a benchmark. AllocsPerRun warms up once (filling the
// state pool) and divides by the run count with integer division, so a
// rare pool miss after a GC can't make it flaky.
func TestRenderAllocs(t *testing.T) {
	type tc struct {
		name string
		tpl  *Template
		data any
	}
	var cases []tc
	for _, sc := range benchdata.Scenarios {
		cases = append(cases, tc{sc.Name, compileScenario(t, sc), sc.Data()})
	}
	esc, _ := Parse("{{s}}")
	cases = append(cases, tc{"escape-only", esc, map[string]any{"s": strings.Repeat("<a href=\"x\">'&'=`</a>", 64)}})
	// builtins nested in a loop recycle their options and frames
	nested, _ := Parse("{{#each items as |it|}}{{#if it.id}}{{#with it}}{{lookup . 'name'}}{{/with}}{{/if}}{{/each}}")
	cases = append(cases, tc{"nested-builtins", nested, benchdata.Scenarios[1].Data()})

	for _, c := range cases {
		n := testing.AllocsPerRun(50, func() {
			if err := c.tpl.RenderTo(io.Discard, c.data); err != nil {
				t.Fatal(err)
			}
		})
		if n != 0 {
			t.Errorf("%s: RenderTo makes %v allocations per render, want 0", c.name, n)
		}
	}
	// Render allocates its result string and nothing else.
	static := cases[0]
	if n := testing.AllocsPerRun(50, func() { static.tpl.Render(static.data) }); n != 1 {
		t.Errorf("static: Render makes %v allocations per render, want 1", n)
	}
}
