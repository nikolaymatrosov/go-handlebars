package bench

// Probes back the "probe" claims about raymond in COMPARISON.md. They log
// what they observe and are skipped by default:
//
//	RAYMOND_PROBES=1 go test -run 'TestProbe' -v .
//	RAYMOND_PROBES=1 go test -race -run TestProbeLazyPartialRace -v .   # reports a data race
//	RAYMOND_PROBE_STACK=1 go test -run TestProbeRecursivePartial -v .  # kills the test process

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aymerick/raymond"
	handlebars "github.com/nikolaymatrosov/go-handlebars"
)

func probe(t *testing.T, env string) {
	if os.Getenv(env) != "1" {
		t.Skip("set " + env + "=1 to run")
	}
}

func ghOut(src string, data any) string {
	t, err := handlebars.Parse(src)
	if err != nil {
		return "error: " + err.Error()
	}
	s, err := t.Render(data)
	if err != nil {
		return "error: " + err.Error()
	}
	return s
}

func rayOut(src string, data any) string {
	var s string
	o := rayCall(func() (err error) { s, err = raymond.Render(src, data); return })
	if o.err != nil {
		return strings.ReplaceAll(o.err.Error(), "\n", " | ")
	}
	return s
}

type probePerson struct {
	FirstName string
	Nick      string `handlebars:"nickname"`
}

func (probePerson) Greet() string { return "hi" }

// TestProbeSemantics: one template per row; both engines' output.
func TestProbeSemantics(t *testing.T) {
	probe(t, "RAYMOND_PROBES")
	for _, c := range []struct {
		src  string
		data any
	}{
		{"{{#with a}}[{{b}}]{{/with}}", map[string]any{"a": map[string]any{"x": 1}, "b": "outer"}},
		{"{{#each items}}[{{b}}]{{/each}}", map[string]any{"items": []any{map[string]any{"x": 1}}, "b": "outer"}},
		{"{{firstName}}|{{FirstName}}|{{nickname}}|{{Greet}}", probePerson{"Ann", "A"}},
		{"{{log 'a' 'b'}}", nil},
		{"{{#each (lookup . 'xs')}}{{.}}{{/each}}", map[string]any{"xs": []any{1, 2}}},
		{"{{#if x includeZero=true}}yes{{/if}}", map[string]any{"x": 0.0}},
		{"{{x}}", map[string]any{"x": 1e21}},
		{"{{x}}", map[string]any{"x": 1e-7}},
		{"{{x}}", map[string]any{"x": []any{1, 2}}},
		{"{{x}}", map[string]any{"x": "&\"'`=<>"}},
		{"{{missing 1}}", nil},
		{"{{x}}", map[string]any{"x": make(chan int)}},
		{"{{f}}", map[string]any{"f": func() (string, error) { return "", nil }}},
	} {
		t.Logf("%q\n\tgo-handlebars: %q\n\traymond:       %q", c.src, ghOut(c.src, c.data), rayOut(c.src, c.data))
	}
}

// TestProbeHelperPanic: a helper that panics with a non-error value or a
// runtime error. raymond's Exec re-raises both, so only the caller's
// recover sees them; go-handlebars returns an error.
func TestProbeHelperPanic(t *testing.T) {
	probe(t, "RAYMOND_PROBES")
	for name, h := range map[string]func() string{
		"panic(string)": func() string { panic("kaboom") },
		"nil-map write": func() string { var m map[string]int; m["x"] = 1; return "" },
	} {
		tpl := raymond.MustParse("{{boom}}")
		tpl.RegisterHelper("boom", h)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Logf("raymond %s: Exec panicked, caller recovered %v", name, p)
				}
			}()
			_, err := tpl.Exec(nil)
			t.Logf("raymond %s: Exec returned err=%v", name, err)
		}()
	}
	gt, _ := handlebars.Parse("{{boom}}")
	gt.RegisterHelper("boom", func(*handlebars.HelperOptions) (any, error) { panic("kaboom") })
	_, err := gt.Render(nil)
	t.Logf("go-handlebars panic(string): errors.Is(err, ErrHelperPanic)=%v", errors.Is(err, handlebars.ErrHelperPanic))
}

// TestProbeGoroutineLeak: raymond lexes in a goroutine feeding an
// unbuffered channel. A lexer error ends that goroutine; a parser error
// leaves it blocked on its next send if input remains.
func TestProbeGoroutineLeak(t *testing.T) {
	probe(t, "RAYMOND_PROBES")
	for _, src := range []string{
		"{{#> p}}x{{/p}}",               // lexer error
		"{{#*inline \"p\"}}{{/inline}}", // lexer error
		"{{foo",                         // lexer error
		"{{#if}}",                       // parser error at end of input
		"{{#a}}{{/b}} and {{c}}",        // parser error, input left
		"{{/a}} and {{c}}",              // parser error, input left
	} {
		runtime.GC()
		before := runtime.NumGoroutine()
		for i := 0; i < 100; i++ {
			if _, err := raymond.Parse(src); err == nil {
				t.Fatalf("%q parsed", src)
			}
		}
		time.Sleep(50 * time.Millisecond)
		t.Logf("%-32q 100 failed parses leave %d goroutines", src, runtime.NumGoroutine()-before)
	}
}

// TestProbeUnbounded: inputs raymond accepts without bound and
// go-handlebars stops with a LimitError at DefaultLimits.
func TestProbeUnbounded(t *testing.T) {
	probe(t, "RAYMOND_PROBES")
	deep := strings.Repeat("{{#a}}", 50000) + strings.Repeat("{{/a}}", 50000)
	start := time.Now()
	_, err := raymond.Parse(deep)
	t.Logf("50,000 nested blocks (%d bytes): raymond err=%v in %v", len(deep), err, time.Since(start).Round(time.Millisecond))
	_, err = handlebars.Parse(deep)
	t.Logf("50,000 nested blocks: go-handlebars err=%v", err)

	x := make([]any, 10)
	for i := range x {
		x[i] = i
	}
	amp := strings.Repeat("{{#each @root.x}}", 6) + "ab" + strings.Repeat("{{/each}}", 6)
	start = time.Now()
	s, err := raymond.Render(amp, map[string]any{"x": x})
	t.Logf("6 nested each over 10 items (%d-byte template): raymond %d bytes, err=%v in %v", len(amp), len(s), err, time.Since(start).Round(time.Millisecond))
	t.Logf("6 nested each over 10 items: go-handlebars %q", ghOut(amp, map[string]any{"x": x}))
}

// TestProbeLazyPartialRace: first Execs of a template whose partial was
// registered from source run concurrently. Run with -race; the race
// detector then fails the test.
func TestProbeLazyPartialRace(t *testing.T) {
	probe(t, "RAYMOND_PROBES")
	tpl := raymond.MustParse("{{> p}}")
	tpl.RegisterPartial("p", "x{{y}}")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tpl.Exec(map[string]any{"y": 1}) }()
	}
	wg.Wait()
}

// TestProbeRecursivePartial: a self-including partial. go-handlebars
// returns a render depth LimitError; raymond recurses until the Go stack
// limit (lowered to 64 MiB here so it fails fast), which is a fatal error
// that kills the process.
func TestProbeRecursivePartial(t *testing.T) {
	probe(t, "RAYMOND_PROBE_STACK")
	gt, _ := handlebars.Parse("{{> self}}")
	gt.RegisterPartial("self", "a{{> self}}")
	_, err := gt.Render(nil)
	fmt.Fprintf(os.Stderr, "go-handlebars: %v\n", err) // t.Log output would die with the process
	debug.SetMaxStack(64 << 20)
	tpl := raymond.MustParse("{{> self}}")
	tpl.RegisterPartial("self", "a{{> self}}")
	_, err = tpl.Exec(nil)
	t.Logf("raymond returned %v", err)
}
