package handlebars

import (
	"io"
	"testing"

	"github.com/nikthespirit/go-handlebars/internal/benchdata"
)

// Run: go test -bench . -benchmem -run '^$'

func compileScenario(tb testing.TB, sc benchdata.Scenario) *Template {
	tb.Helper()
	t, err := Parse(sc.Template)
	if err != nil {
		tb.Fatal(err)
	}
	for name, src := range sc.Partials {
		if err := t.RegisterPartial(name, src); err != nil {
			tb.Fatal(err)
		}
	}
	return t
}

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
