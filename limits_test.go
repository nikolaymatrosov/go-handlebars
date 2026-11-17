package handlebars

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
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
		{"dynamic name recursion", "{{> (lookup . 'n')}}", map[string]string{"a": "{{> (lookup . 'n')}}"}, "render depth"},
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
		err = bounded(t, func() error { _, err := tpl.Render(map[string]any{"n": "a"}); return err })
		var le *LimitError
		if !errors.As(err, &le) || le.What != c.what {
			t.Errorf("%s: err = %v, want %s LimitError", c.name, err, c.what)
		}
	}
}

// bounded runs f, failing the test if it hasn't returned after a timeout
// far above what a limited render takes (milliseconds), so a bomb a limit
// fails to stop fails the test instead of hanging CI.
func bounded(t *testing.T, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("render still running after 10s: a limit failed to stop it")
		return nil
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

// A partial block or an inline partial binds a copy of its scope, which is
// O(depth): deep in blocks, making and running one costs an op per 8
// entries copied, so MaxOps bounds the bytes copied too. Uncharged, each
// case fit MaxOps, and at default limits the first took 2.7 s and 12 GB.
func TestDeepClosureOps(t *testing.T) {
	const maxOps = 100_000
	ifs, endifs := strings.Repeat("{{#if 1}}", 80), strings.Repeat("{{/if}}", 80)
	for _, src := range []string{
		ifs + strings.Repeat("{{#> nope}}{{/nope}}", 20_000) + endifs,                                    // ~1 op per call uncharged
		ifs + `{{#*inline "q"}}{{#if 1}}{{/if}}{{/inline}}` + strings.Repeat("{{> q}}", 20_000) + endifs, // ~4 ops per call uncharged
	} {
		tpl, err := ParseWithOptions(src, Options{Limits: Limits{MaxOps: maxOps}})
		if err != nil {
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err = tpl.Render(nil)
		runtime.ReadMemStats(&after)
		var le *LimitError
		if !errors.As(err, &le) || le.What != "ops" {
			t.Errorf("%.30s...: err = %v, want ops LimitError", src[len(ifs):], err)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > maxOps<<10 {
			t.Errorf("%.30s...: allocated %d bytes, want at most 1 KiB per op", src[len(ifs):], n)
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

func TestLimitParseDepth(t *testing.T) {
	nest := func(open, close string) string { return strings.Repeat(open, 2000) + strings.Repeat(close, 2000) }
	for _, src := range []string{
		nest("{{#if x}}", "{{/if}}"),
		"{{a " + nest("(b ", ")") + "}}",
		nest("{{#> p}}", "{{/p}}"),
		"{{#if a}}" + strings.Repeat("{{else if a}}", 2000) + "{{/if}}",
	} {
		_, err := Parse(src)
		var le *LimitError
		if !errors.As(err, &le) || le.What != "parse depth" || le.Max != DefaultLimits.MaxParseDepth {
			t.Errorf("%.20s...: got %v, want parse depth LimitError", src, err)
		}
	}
	// Depth is nesting, not count: siblings parse, and MaxParseDepth levels fit.
	if _, err := Parse(strings.Repeat("{{#if x}}{{/if}}{{a (b (c))}}", 2000)); err != nil {
		t.Errorf("siblings: %v", err)
	}
	d := DefaultLimits.MaxParseDepth
	if _, err := Parse(strings.Repeat("{{#if x}}", d) + strings.Repeat("{{/if}}", d)); err != nil {
		t.Errorf("%d nested blocks: %v", d, err)
	}
	for n, want := range map[int]bool{5: true, 6: false} {
		_, err := ParseWithOptions(strings.Repeat("{{#if x}}", n)+strings.Repeat("{{/if}}", n), Options{Limits: Limits{MaxParseDepth: 5}})
		if (err == nil) != want {
			t.Errorf("%d nested blocks, MaxParseDepth 5: %v", n, err)
		}
	}
}

func TestLimitTemplateSize(t *testing.T) {
	_, err := Parse(strings.Repeat("x", DefaultLimits.MaxTemplateSize+1))
	var le *LimitError
	if !errors.As(err, &le) || le.What != "template size" || le.Max != DefaultLimits.MaxTemplateSize {
		t.Errorf("got %v, want template size LimitError", err)
	}
	if _, err := Parse(strings.Repeat("x", DefaultLimits.MaxTemplateSize)); err != nil {
		t.Errorf("exactly MaxTemplateSize: %v", err)
	}
	// Partials are parsed with the template's Options, limits included.
	tpl, _ := ParseWithOptions("", Options{Limits: Limits{MaxTemplateSize: 10}})
	if err := tpl.RegisterPartial("p", "01234567890"); !errors.As(err, &le) || le.What != "template size" {
		t.Errorf("partial: got %v, want template size LimitError", err)
	}
}

// countWriter counts the bytes written to it.
type countWriter struct{ n int }

func (w *countWriter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

// Billion laughs: nested loops multiply output. The bytes a helper's FnTo
// renders into its own writer count too, even though it drops them.
func TestLimitOutputSize(t *testing.T) {
	const max = 1 << 16
	big := map[string]any{"a": make([]any, 1000)}
	for _, src := range []string{
		"{{#each a}}{{#each ../a}}xxxxxxxxxx{{/each}}{{/each}}",  // 10 MB
		"{{#each a}}{{#discard}}xxxxxxxxxx{{/discard}}{{/each}}", // 1 MB into io.Discard
	} {
		tpl, err := ParseWithOptions(src, Options{Limits: Limits{MaxOutputSize: max}})
		if err != nil {
			t.Fatal(err)
		}
		tpl.RegisterHelper("discard", func(o *HelperOptions) (any, error) {
			for i := 0; i < 100; i++ {
				if err := o.FnTo(io.Discard, o.Ctx(), nil, nil); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		var w countWriter
		for _, err := range []error{
			bounded(t, func() error { _, err := tpl.Render(big); return err }),
			bounded(t, func() error { return tpl.RenderTo(&w, big) }),
		} {
			var le *LimitError
			if !errors.As(err, &le) || le.What != "output size" || le.Max != max {
				t.Errorf("%s: got %v, want output size LimitError", src, err)
			}
		}
		if w.n > max {
			t.Errorf("%s: RenderTo wrote %d bytes, max %d", src, w.n, max)
		}
	}
}

func TestLimitOps(t *testing.T) {
	tpl, _ := ParseWithOptions("{{#each a}}{{#each ../a}}{{/each}}{{/each}}", Options{Limits: Limits{MaxOps: 1000}})
	err := bounded(t, func() error { _, err := tpl.Render(map[string]any{"a": make([]any, 1000)}); return err })
	var le *LimitError
	if !errors.As(err, &le) || le.What != "ops" || le.Max != 1000 {
		t.Errorf("got %v, want ops LimitError", err)
	}

	// Each decorator run is one op: n decorators in one wrap fit MaxOps n,
	// not n-1 (the decorator statements themselves render nothing).
	const n = 50
	for max, want := range map[int]bool{n: true, n - 1: false} {
		tpl, _ := ParseWithOptions(strings.Repeat("{{*d}}", n), Options{Limits: Limits{MaxOps: max}})
		tpl.registerDecorator("d", func(map[string]any, *HelperOptions) error { return nil })
		_, err := tpl.Render(nil)
		if (err == nil) != want || err != nil && (!errors.As(err, &le) || le.What != "ops") {
			t.Errorf("%d decorators, MaxOps %d: %v", n, max, err)
		}
	}
}

// RenderCtx notices a cancel that happens mid-render within 1024 ops.
func TestRenderCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tpl, _ := Parse("{{#each items}}{{tick}}{{/each}}")
	calls := 0
	tpl.RegisterHelper("tick", func(*HelperOptions) (any, error) {
		if calls++; calls == 10 {
			cancel()
		}
		return nil, nil
	})
	err := tpl.RenderCtx(ctx, io.Discard, map[string]any{"items": make([]any, 100_000)})
	if !errors.Is(err, context.Canceled) || calls >= 1024 { // 2 ops per item: stops within ~512 more
		t.Errorf("err = %v after %d calls, want context.Canceled within 1024 ops", err, calls)
	}
}

type helperErr struct{ msg string }

func (e *helperErr) Error() string { return e.msg }

// failWriter fails every Write.
type failWriter struct{ writes int }

var errWrite = errors.New("write failed")

func (w *failWriter) Write([]byte) (int, error) { w.writes++; return 0, errWrite }

// Errors from helpers and from io.Writers reach the caller unchanged.
func TestErrorsSurface(t *testing.T) {
	errSentinel := errors.New("sentinel")
	tpl, _ := Parse(`{{#each a}}{{> p}}{{/each}}`)
	tpl.RegisterPartial("p", "{{#with .}}{{fail}}{{/with}}")
	tpl.RegisterHelper("fail", func(*HelperOptions) (any, error) { return nil, errSentinel })
	if _, err := tpl.Render(map[string]any{"a": []any{1}}); !errors.Is(err, errSentinel) {
		t.Errorf("nested helper error: %v", err)
	}
	tpl.RegisterHelper("fail", func(*HelperOptions) (any, error) { return nil, &helperErr{"typed"} })
	var he *helperErr
	if _, err := tpl.Render(map[string]any{"a": []any{1}}); !errors.As(err, &he) || he.msg != "typed" {
		t.Errorf("typed helper error: %v", err)
	}

	// The final flush, a flush mid-render (which stops it) and a helper's
	// own FnTo writer.
	small, _ := Parse("x")
	big, _ := Parse("{{#each a}}xxxxxxxxxx{{/each}}")
	fnTo, _ := Parse("{{#tee}}x{{/tee}}")
	fnTo.RegisterHelper("tee", func(o *HelperOptions) (any, error) { return nil, o.FnTo(&failWriter{}, o.Ctx(), nil, nil) })
	data := map[string]any{"a": make([]any, 10_000)}
	for _, tpl := range []*Template{small, big, fnTo} {
		w := &failWriter{}
		if err := tpl.RenderTo(w, data); !errors.Is(err, errWrite) || w.writes > 1 {
			t.Errorf("writer error: %v after %d writes", err, w.writes)
		}
	}
}

// A recovered panic inside nested blocks and partials restores the render
// state: a helper that drops the error carries on with the right context
// stack, block params, @data, inline partials and depth. The panic (in a
// KeyedCollection's Get, so not under a helper's own recover) unwinds
// library frames up to the wrap helper's recover.
func TestPanicStateRestored(t *testing.T) {
	tpl, err := Parse(`{{#*inline "q"}}q{{/inline}}{{#each items as |it|}}` +
		`{{#try}}{{#with .}}{{#wrap}}{{#each ../items}}{{> p @root}}{{/each}}{{/wrap}}{{/with}}{{/try}}` +
		`<{{it}}{{@index}}{{../top}}{{> q}}>{{/each}}`)
	if err != nil {
		t.Fatal(err)
	}
	tpl.RegisterPartial("p", "{{#with kc}}{{x}}{{/with}}")
	tpl.RegisterHelper("wrap", func(o *HelperOptions) (any, error) { return o.Fn() })
	tpl.RegisterHelper("try", func(o *HelperOptions) (any, error) {
		if _, err := o.Fn(); !errors.Is(err, ErrHelperPanic) {
			return nil, fmt.Errorf("want a recovered panic, got %v", err)
		}
		return "!", nil
	})
	items := make([]any, 150) // a leaked render depth would trip MaxRenderDepth
	var want strings.Builder
	for i := range items {
		items[i] = i
		fmt.Fprintf(&want, "!<%d%dTq>", i, i)
	}
	got, err := tpl.Render(map[string]any{"top": "T", "items": items, "kc": panicKC{}})
	if got != want.String() || err != nil {
		t.Errorf("got %.80q, %v\nwant %.80q", got, err, want.String())
	}
}

// A standalone partial's indent repeats on every line it renders, so an
// n-space indent over n lines (`<n spaces>{{> p}}` with p n newlines) is
// n^2 bytes: the writer must stop at MaxOutputSize, not build it all first.
func TestIndentBomb(t *testing.T) {
	lines := strings.Repeat("\n", 1<<10)
	for _, write := range []func(o *outWriter) error{
		func(o *outWriter) error { return o.writeString(lines) },
		func(o *outWriter) error { return o.writeValue(lines, false) },
	} {
		n := 0
		o := &outWriter{emitted: &n, max: 1 << 10}
		o.pushIndent(strings.Repeat(" ", 1<<10)) // 1 MiB indented
		err := write(o)
		var le *LimitError
		if !errors.As(err, &le) || le.What != "output size" || cap(o.buf) > 1<<12 {
			t.Errorf("err = %v, buffer grew to %d bytes", err, cap(o.buf))
		}
	}
}

// Work a statement does in proportion to its own size costs ops, so a big
// statement in a loop or a fan-out partial can't multiply it for free:
// arguments, hash pairs, path segments (cyclic data never ends a path)
// and comments.
func TestStatementSizeOps(t *testing.T) {
	cyc := map[string]any{}
	cyc["a"] = cyc
	for _, src := range []string{
		"{{lookup a" + strings.Repeat(" a", 1000) + "}}",
		"{{lookup a" + strings.Repeat(" k=a", 1000) + "}}",
		"{{> p" + strings.Repeat(" k=a", 1000) + "}}",
		"{{a" + strings.Repeat(".a", 1000) + "}}",
		strings.Repeat("{{!}}", 1000),
	} {
		for _, strict := range []bool{false, true} {
			tpl, err := ParseWithOptions(src, Options{Strict: strict, Limits: Limits{MaxOps: 500}})
			if err != nil {
				t.Fatal(err)
			}
			if err := tpl.RegisterPartial("p", ""); err != nil {
				t.Fatal(err)
			}
			_, err = tpl.Render(cyc)
			var le *LimitError
			if !errors.As(err, &le) || le.What != "ops" {
				t.Errorf("%.20s... (strict %v): err = %v, want ops LimitError", src, strict, err)
			}
		}
	}
}

// Every KiB of a long template string a statement hashes, compares or
// copies costs an op, so a statement with a huge name can't do O(len)
// work per op: path segments, string literals, hash keys, partial names
// and inline-partial names of the same length.
func TestLongNameOps(t *testing.T) {
	k := strings.Repeat("k", 600<<10) // 600 ops per use
	half := strings.Repeat("k", 300<<10)
	for _, c := range []struct {
		src    string
		maxOps int
	}{
		{"{{m.[" + k + "]}}", 500},
		{`{{lookup m "` + k + `"}}`, 500},
		{"{{lookup m " + k + "=1}}", 500},
		{"{{> p " + k + "=1}}", 500},
		{"{{> [" + k + "]}}", 500},
		// 300 to define, 300 to look up, 300 to compare the equal-length names
		{`{{#*inline "` + half + `a"}}{{/inline}}{{> "` + half + `b"}}`, 750},
	} {
		tpl, err := ParseWithOptions(c.src, Options{Limits: Limits{MaxOps: c.maxOps}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tpl.RegisterPartial("p", ""); err != nil {
			t.Fatal(err)
		}
		_, err = tpl.Render(map[string]any{"m": map[string]any{}})
		var le *LimitError
		if !errors.As(err, &le) || le.What != "ops" {
			t.Errorf("%.24s...: err = %v, want ops LimitError", c.src, err)
		}
	}
}

// Under TrackIDs, contextPath and block-param ids are copied per item and
// per call, and one id can be as long as the template (`this/./././...`
// drops the dots from the lookup but keeps them in the id): each KiB
// copied costs an op. Uncharged, each case stays under MaxOps.
func TestTrackIDsPathOps(t *testing.T) {
	long := "this" + strings.Repeat("/.", 100_000) // a 200 KB id
	data := map[string]any{"a": make([]any, 250), "b": make([]any, 1000)}
	for _, c := range []struct {
		src  string
		data any
	}{
		{"{{#with " + long + "}}{{#each a}}{{#each ../a}}x{{/each}}{{/each}}{{/with}}", data}, // the review's probe
		{"{{#with " + long + "}}{{#each b}}x{{/each}}{{/with}}", data},                        // each: per item
		{"{{#with " + long + "}}{{#rep}}{{#with .}}x{{/with}}{{/rep}}{{/with}}", data},        // with: appendContextPath
		{"{{#with " + long + " as |w|}}{{#rep}}{{lookup w.x 1}}{{/rep}}{{/with}}", data},      // a block param's id
		{"{{#rep}}{{#each " + long + "}}{{/each}}{{/rep}}", map[string]any{}},                 // each start: no contextPath, no items
	} {
		src := c.src
		tpl, err := ParseWithOptions(src, Options{TrackIDs: true, Limits: Limits{MaxOps: 100_000}})
		if err != nil {
			t.Fatal(err)
		}
		tpl.RegisterHelper("rep", func(o *HelperOptions) (any, error) {
			for i := 0; i < 1000; i++ {
				if _, err := o.Fn(); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		err = bounded(t, func() error {
			_, err := tpl.Render(c.data)
			return err
		})
		var le *LimitError
		if !errors.As(err, &le) || le.What != "ops" {
			t.Errorf("%.60s: err = %v, want ops LimitError", strings.Replace(src, long, "this/./...", 1), err)
		}
	}
}

// MaxExpressions counts one per helper-call shape. Each row parses at its
// count and fails below it. A limit of 0 means the default, so the lower
// bound of a count-1 row is checked on the row repeated twice.
func TestLimitExpressions(t *testing.T) {
	cases := []struct {
		src string
		n   int
	}{
		{"text {{! comment }}", 0},
		{"{{a}}", 1},
		{"{{a b c d=e}}", 1},
		{"{{a (b c)}}", 2},
		{"{{#if a}}x{{else if b}}y{{else}}z{{/if}}", 2},
		{"{{^a}}x{{/a}}", 1},
		{"{{> p}}", 1},
		{`{{> (lookup . "k")}}`, 2},
		{"{{#> p}}x{{/p}}", 1},
		{"{{{{raw}}}} {{x}} {{{{/raw}}}}", 1},
		{"{{* deco}}", 1},
		{`{{#*inline "n"}}{{x}}{{/inline}}`, 2},
	}
	parse := func(src string, max int) error {
		_, err := ParseWithOptions(src, Options{Limits: Limits{MaxExpressions: max}})
		return err
	}
	isLimit := func(err error, max int) bool {
		var le *LimitError
		return errors.As(err, &le) && le.What == "expressions" && le.Max == max
	}
	for _, c := range cases {
		if c.n == 0 { // counts nothing: one more expression fits a limit of 1
			if err := parse(c.src+"{{a}}", 1); err != nil {
				t.Errorf("%q plus one expression at MaxExpressions 1: %v", c.src, err)
			}
			continue
		}
		if err := parse(c.src, c.n); err != nil {
			t.Errorf("%q at MaxExpressions %d: %v", c.src, c.n, err)
		}
		if c.n > 1 {
			if err := parse(c.src, c.n-1); !isLimit(err, c.n-1) {
				t.Errorf("%q at MaxExpressions %d: got %v, want expressions LimitError", c.src, c.n-1, err)
			}
		}
		twice := c.src + c.src
		if err := parse(twice, 2*c.n); err != nil {
			t.Errorf("%q at MaxExpressions %d: %v", twice, 2*c.n, err)
		}
		if err := parse(twice, 2*c.n-1); !isLimit(err, 2*c.n-1) {
			t.Errorf("%q at MaxExpressions %d: got %v, want expressions LimitError", twice, 2*c.n-1, err)
		}
	}

	// The default applies to a zero field.
	d := DefaultLimits.MaxExpressions
	if d != 100_000 {
		t.Errorf("DefaultLimits.MaxExpressions = %d, want 100000", d)
	}
	if _, err := Parse(strings.Repeat("{{a}}", d)); err != nil {
		t.Errorf("%d expressions: %v", d, err)
	}
	if _, err := Parse(strings.Repeat("{{a}}", d+1)); !isLimit(err, d) {
		t.Errorf("%d expressions: got %v, want expressions LimitError", d+1, err)
	}

	// Lexing stops at the first expression over the limit, so a source far
	// over it costs only its prefix. Counted after the whole source was
	// tokenized, this allocated over 150 MB.
	src := strings.Repeat("{{a}}", 200_000) // 977 KiB
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := ParseWithOptions(src, Options{Limits: Limits{MaxExpressions: 100}})
	runtime.ReadMemStats(&after)
	if !isLimit(err, 100) {
		t.Errorf("200,000 expressions at MaxExpressions 100: got %v, want expressions LimitError", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Errorf("parse allocated %d bytes before stopping, want under 1 MiB", n)
	}

	// RegisterPartial parses with the template's Options: each partial has
	// its own budget.
	tpl, err := ParseWithOptions("{{> p}}", Options{Limits: Limits{MaxExpressions: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tpl.RegisterPartial("p", "{{a}}"); err != nil {
		t.Errorf("partial within its budget: %v", err)
	}
	if err := tpl.RegisterPartial("q", "{{a}}{{b}}"); !isLimit(err, 1) {
		t.Errorf("partial over its budget: got %v, want expressions LimitError", err)
	}
}
