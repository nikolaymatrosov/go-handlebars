// Package bench compares go-handlebars with github.com/aymerick/raymond:
// benchmarks over the library's own scenarios (internal/benchdata) and a
// run of the handlebars-spec subset that needs no JS code (spec_test.go).
// It is a separate module so the library's go.mod stays dependency-free.
//
//	go test -bench . -benchmem -count=5 -run '^$'
package bench

import (
	"strings"
	"testing"

	"github.com/aymerick/raymond"
	handlebars "github.com/nikthespirit/go-handlebars"
	"github.com/nikthespirit/go-handlebars/internal/benchdata"
)

type renderFunc func(data any) (string, error)

var engines = []struct {
	name    string
	compile func(benchdata.Scenario) (renderFunc, error)
}{
	{"go-handlebars", compileGH},
	{"raymond", compileRaymond},
}

func compileGH(sc benchdata.Scenario) (renderFunc, error) {
	t, err := handlebars.Parse(sc.Template)
	if err != nil {
		return nil, err
	}
	for name, src := range sc.Partials {
		if err := t.RegisterPartial(name, src); err != nil {
			return nil, err
		}
	}
	return t.Render, nil
}

// compileRaymond parses the partials up front: raymond's RegisterPartial
// defers parsing to the first Exec, which would leave it out of Parse.
func compileRaymond(sc benchdata.Scenario) (renderFunc, error) {
	t, err := raymond.Parse(sc.Template)
	if err != nil {
		return nil, err
	}
	for name, src := range sc.Partials {
		p, err := raymond.Parse(src)
		if err != nil {
			return nil, err
		}
		t.RegisterPartialTemplate(name, p)
	}
	return t.Exec, nil
}

// sameEscapes rewrites the characters the engines escape differently:
// raymond writes ' as &apos; and leaves ` and = as they are, where
// handlebars.js (and go-handlebars) write &#x27; &#x60; &#x3D;. Each becomes
// a character both escape alike, so data keeps its length and escape
// density. Only the escape scenario's data contains any of them.
var sameEscapes = strings.NewReplacer("'", `"`, "`", "<", "=", ">")

func scrub(v any) any {
	switch x := v.(type) {
	case string:
		return sameEscapes.Replace(x)
	case []any:
		for i := range x {
			x[i] = scrub(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = scrub(x[k])
		}
	}
	return v
}

type compiled struct {
	data   any
	render []renderFunc // per engine
}

// compile compiles sc with every engine and fails unless all engines
// render identical output. Benchmarks compile one scenario at a time, so
// the live heap holds only its data (as in the library's own benchmarks).
func compile(tb testing.TB, sc benchdata.Scenario) compiled {
	c := compiled{data: scrub(sc.Data())}
	var want string
	for i, e := range engines {
		r, err := e.compile(sc)
		if err != nil {
			tb.Fatalf("%s/%s: compile: %v", sc.Name, e.name, err)
		}
		got, err := r(c.data)
		if err != nil {
			tb.Fatalf("%s/%s: render: %v", sc.Name, e.name, err)
		}
		if i == 0 {
			want = got
		} else if got != want {
			tb.Fatalf("%s: %s output differs from %s:\n%s\n---\n%s", sc.Name, e.name, engines[0].name, got, want)
		}
		c.render = append(c.render, r)
	}
	return c
}

func TestSameOutput(t *testing.T) {
	for _, sc := range benchdata.Scenarios {
		compile(t, sc)
	}
}

// BenchmarkRender renders each scenario to a string (go-handlebars Render,
// raymond Exec).
func BenchmarkRender(b *testing.B) {
	for _, sc := range benchdata.Scenarios {
		c := compile(b, sc)
		for i, e := range engines {
			r := c.render[i]
			b.Run(sc.Name+"/"+e.name, func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					if _, err := r(c.data); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkParse is the compile cost: the template and its partials.
func BenchmarkParse(b *testing.B) {
	for _, sc := range benchdata.Scenarios {
		compile(b, sc)
		for _, e := range engines {
			b.Run(sc.Name+"/"+e.name, func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					if _, err := e.compile(sc); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
