# go-handlebars

go-handlebars is a [Handlebars](https://handlebarsjs.com) template engine for Go. It renders templates the way handlebars.js 4.7.9 does and passes all 632 tests of the handlebars-spec corpus. It is built for templates written by people you don't fully trust: every render is bounded by size, depth, output and work limits. It has no dependencies outside the standard library.

```sh
go get github.com/nikthespirit/go-handlebars
```

It requires Go 1.22 or later.

## Usage

```go
tpl, err := handlebars.Parse(`<h1>{{title}}</h1>
<ul>
{{#each items}}
  {{> item}}
{{/each}}
</ul>
`)
if err != nil {
	panic(err)
}
if err := tpl.RegisterPartial("item", "<li>{{shout name}}{{#if @last}} (last){{/if}}</li>\n"); err != nil {
	panic(err)
}
tpl.RegisterHelper("shout", func(o *handlebars.HelperOptions) (any, error) {
	return strings.ToUpper(fmt.Sprint(o.Args[0])) + "!", nil
})

out, err := tpl.Render(map[string]any{
	"title": "Fish & chips",
	"items": []any{
		map[string]any{"name": "cod"},
		map[string]any{"name": "haddock"},
	},
})
// out:
// <h1>Fish &amp; chips</h1>
// <ul>
//   <li>COD!</li>
//   <li>HADDOCK! (last)</li>
// </ul>
```

This is `Example` in [example_test.go](example_test.go), so `go test` keeps it compiling and checks its output. `Example_untrusted` in the same file shows limits and cancellation.

### API at a glance

- **Parsing.** `Parse(src)` and `ParseWithOptions(src, Options)`. `Options` mirrors the handlebars.js compile options: `Strict`, `AssumeObjects`, `Compat`, `Data`, `KnownHelpers`, `KnownHelpersOnly`, `IgnoreStandalone`, `PreventIndent`, `ExplicitPartialContext`, `NoEscape`, and the legacy `StringParams` and `TrackIDs`. It also carries `Limits`.
- **Rendering.**
  - `Render(data)` returns a string.
  - `RenderTo(w, data)` writes to an `io.Writer`.
  - `RenderCtx(ctx, w, data)` adds cancellation.
  - `RenderWithData(w, data, vars)` seeds `@data` variables (handlebars.js `runtimeOptions.data`).
- **Helpers.**
  - `RegisterHelper(name, func(*HelperOptions) (any, error))`.
  - Inside a helper:
    - `o.Args` and `o.Hash` hold the evaluated arguments.
    - `o.Ctx()` is `this`.
    - `o.Fn()`, `o.FnWith(ctx)` and `o.Inverse()` render the block or its `{{else}}`. `o.FnTo` and `o.InverseTo` write straight to a writer.
    - `o.DataVar("index")` reads `@index`, and `o.BlockParamNames()` returns the block's `as |…|` names.
  - Return a `SafeString` to skip HTML escaping.
  - Registering a helper named like a builtin replaces it. That includes the `helperMissing` and `blockHelperMissing` hooks.
- **Partials.**
  - `RegisterPartial(name, src)` parses `src` with the template's Options.
  - `RegisterPartialTemplate(name, *Template)` registers a template you already parsed.
  - `RegisterPartialFunc(name, func(ctx any) (string, error))` registers a partial implemented in Go.
  - Templates can also use inline partials (`{{#*inline}}`), partial blocks (`{{#> p}}…{{/p}}`, `@partial-block`) and dynamic partials (`{{> (lookup . 'kind')}}`).
- **`{{log}}`.** Output goes to `Template.Logger func(level string, args ...any)`. When it is nil, the output is dropped.
- **Errors.**
  - `*Error` reports parse and runtime errors with `Line`, which is 1-based, and `Column`, which is the 0-based JavaScript column. The text reads like a handlebars.js exception, e.g. `Missing helper: "x"`.
  - `*LimitError` reports a hit limit.
  - A panic in your code is returned as an error wrapping `ErrHelperPanic`, together with its stack. "Your code" means helpers, hooks, function values in the data, and the Logger.

## Security model

go-handlebars assumes the template may be hostile. The data and helpers are assumed to be yours. Every `Template` has `Limits`; a zero field means its `DefaultLimits` value.

| Limit | Default | What it stops |
| --- | --- | --- |
| `MaxTemplateSize` | 1 MiB | Huge sources. `Parse` rejects them before scanning. `RegisterPartial` applies the same check to each partial's source. |
| `MaxParseDepth` | 100 | Stack exhaustion while parsing, from deep nesting of blocks, subexpressions and partial blocks. Each `{{else if …}}` link in a chain counts as one level, so a chain can have at most 99 links at the default. |
| `MaxRenderDepth` | 100 | Recursive and mutually recursive partials, including dynamic names like `{{> (lookup . 'next')}}`, and deep runtime nesting. Block scopes and partial expansions count together. |
| `MaxOutputSize` | 8 MiB | Output amplification, such as nested `{{#each}}` "billion laughs" or a large indent applied to many lines. Every rendered byte counts. That includes the intermediate buffers a helper renders with `Fn()`/`FnWith()`/`Inverse()` and bytes it writes with `FnTo` into its own writer, even if it then discards them. |
| `MaxOps` | 1,000,000 | CPU exhaustion. See the list below. |

These count one op each:
- a substitution;
- a helper call, or a `Fn`/`FnTo` call;
- a loop iteration;
- an argument or hash pair;
- each path segment after the first;
- a comment;
- a partial call;
- an inline partial bound or scanned;
- a decorator;
- a key that a hash partial copies.

On top of those, the render charges one op per KiB of a long name or `trackIds` path that it hashes or copies, and one op per 8 entries of scope (contexts and block params) that a partial block or inline partial copies when it is bound or run. Work per op is bounded, so `MaxOps` bounds the whole render. For scale, the benchmark's `{{#each}}` over 1000 items with three substitutions per item uses 4,002 ops, which is 0.4% of the default.

Further guarantees:

- **Sticky limits.** Once a render exceeds a limit, it fails, even if a helper catches the `LimitError` from `Fn()` and returns normally.
- **No library panics.** The library returns errors instead of panicking on any template, data or option combination. `FuzzParse` and `FuzzRender` in `fuzz_test.go` check this. A panic in user code becomes an `ErrHelperPanic` error.
- **Cancellation.** `RenderCtx` checks `ctx.Err()` before rendering starts and then every 1024 ops, and returns that error.
  - A helper that drops the context error doesn't hide the cancellation: the render sees it again at the next check.
  - Cancellation is a stop signal, not a limit, so it is not sticky. If fewer than 1024 ops remain when a helper drops the error, the render can still complete.
- **Partial templates.** A partial registered with `RegisterPartialTemplate` renders with its own parse-time Options, such as `Strict` or `NoEscape`. `Limits` and `Data` always come from the template being rendered.

Known residual costs. Both depend on the shape of the data, not the template alone, and each stays within a constant factor per op:

- Formatting a deeply nested array as a key or in an error message is capped at 1 KiB of output, but 64-deep single-element arrays can still cost up to about 64k reflection steps per op. This applies to `lookup` keys and strict-mode error text.
- A long string from the data used as a lookup key is hashed on each use. It is not charged by length; long strings from the template are.

The fuzzers bound each input to 2 s, so they cannot catch an amplification that stays below that under their small limits.

If the data is untrusted too, check its size and nesting before rendering.

## Spec compliance

go-handlebars passes all 632 tests of [handlebars-spec](https://github.com/jbboehr/handlebars-spec) at commit `748917a`, which corresponds to handlebars.js v4.7.9. That covers the tokenizer, parser and render suites. Where a test names the expected error, the message must match it exactly or match its regular expression. The corpus's JavaScript helpers are ported to Go in `spec_funcs_test.go`. To run the suite:

```sh
git submodule update --init
go test -run TestSpec -v . | grep -E 'json:|spec total'
# ...
# spec total: 632/632 pass, 0 fail, 0 skip
```

`go test ./...` fails if any spec file drops below its pass gate.

## Concurrency

- **Rendering.** A parsed `Template` is safe for concurrent `Render`, `RenderTo`, `RenderCtx` and `RenderWithData`. `TestRenderParallel` checks this under `-race`. Your helpers then run concurrently too, so they must be safe for that.
- **Registration.** `RegisterHelper`, `RegisterPartial*` and setting `Logger` are not safe to run concurrently with anything. Do all of it before the first render.
- **HelperOptions.** A `*HelperOptions` is valid only during the helper call. Keeping it is unsupported but isolated: a render that hands one to user code does not recycle its internal state, so a stale `HelperOptions` cannot read or corrupt another render.

## Data model

- **Objects.**
  - `map[string]any`, and any map whose key kind is string.
  - Structs, through their exported fields, matched by the exact Go field name. There is no `json` tag mapping, and methods are not called.
  - `KeyedCollection` (`Keys`, `Get`, `Len`) for objects that need a key order.
- **`{{#each}}` order.**
  - A `KeyedCollection` iterates in `Keys()` order, which you need to reproduce JavaScript insertion order.
  - A plain Go map iterates in sorted key order, so output is deterministic.
  - A struct iterates in field order.
- **Arrays.** Slices and arrays, with numeric indices and `length`. Strings have `length` too, in UTF-16 units, as in JavaScript.
- **Pointers and interfaces** are followed. A nil value is JavaScript null/undefined.
- **Printing.** Values print as JavaScript `String(v)` would:
  - numbers as `Number#toString` (`1e+21`, `0.25`);
  - arrays joined with commas;
  - maps and structs as `[object Object]`;
  - nil as an empty string.

  `fmt.Stringer` is not consulted.
- **Truthiness.** It follows JavaScript, so an empty map is truthy. `{{#if}}` treats an empty array as false.
- **Function values.** A value of type `Helper` (`func(*HelperOptions) (any, error)`) found by a lookup is called with the current context as `o.Ctx()`, like a function-valued property in JavaScript. Other function types are not called.

## Deviations from handlebars.js

- **Helpers and hooks.**
  - There is one helper signature, `func(*HelperOptions) (any, error)`. Function values in the data must have it to be called.
  - `HelperOptions.Hash` is nil when the call has no hash arguments; JavaScript passes `{}`. Read it, but don't write to it.
  - `FnTo`'s `dataVars` add a child `@data` frame, like `createFrame` plus assignments. A JavaScript `fn(ctx, {data: obj})` that replaces the frame wholesale cannot be expressed.
  - `HelperOptions.Lookup(path)` takes a dot-separated property chain only; `../`, `this` and `[…]` are not accepted.
  - `{{log}}` writes to `Template.Logger` instead of the console.
  - Panics in user code are returned as errors (`ErrHelperPanic`).
- **Iteration.** `{{#each}}` iterates arrays and objects only; a string or number takes `{{else}}`. Plain Go maps iterate in sorted key order; channels and iterators are not supported.
- **Partials.**
  - Each template has one partial registry. handlebars.js's global partials and per-render partials both go there.
  - `RegisterPartialFunc` partials receive the context only, not the JavaScript `options` argument.
  - Inline partials and partial blocks see block params at the depth where they are written. JavaScript can be off by one there.
  - `{{@partial-block}}` used as a value renders as an object.
- **Decorators.** There is no public decorator API, since decorators are deprecated in handlebars.js 4. The built-in `{{#*inline}}` works.
- **Errors.** `Error.Column` is the 0-based JavaScript column.
- **Legacy options.** Under `StringParams`/`TrackIDs`, helpers get the extra data in `HelperOptions.Legacy`, and `FnToPaths` passes block-param paths. IDs are JavaScript `String(id)`: `"null"`, `"true"` or `"undefined"` for literals and subexpressions.

## Performance

These are medians of 5 runs, comparing `Render` with [raymond](https://github.com/aymerick/raymond) v2.0.2's `Exec` on the same scenarios and inputs (go1.25.10, Apple M4 Pro). Both engines produce byte-identical output. For that, the escape data avoids the three characters the two engines escape differently (see [§3 of COMPARISON.md](bench/COMPARISON.md#3-benchmarks)). Parse includes the partials.

| Benchmark | go-handlebars | raymond |
| --- | --- | --- |
| Render/static (6 KiB page) | 671 ns, 6,148 B, 1 alloc | 2,369 ns, 19,206 B, 28 allocs |
| Render/each (1000 items) | 142 µs, 40,986 B, 1 alloc | 3,352 µs, 18,766,396 B, 35,535 allocs |
| Render/partials (100 × 3 partials) | 31.6 µs, 12,296 B, 1 alloc | 266 µs, 859,861 B, 7,108 allocs |
| Render/escape | 37.9 µs, 57,384 B, 1 alloc | 205 µs, 1,205,147 B, 804 allocs |
| Parse/static | 6.2 µs, 2,896 B, 26 allocs | 869 µs, 2,006 B, 46 allocs |
| Parse/each | 2.0 µs, 5,952 B, 50 allocs | 13.6 µs, 3,959 B, 96 allocs |
| Parse/partials | 4.3 µs, 11,360 B, 113 allocs | 33.3 µs, 9,126 B, 216 allocs |
| Parse/escape | 1.5 µs, 3,624 B, 40 allocs | 10.3 µs, 3,070 B, 74 allocs |

`Render`'s one allocation is the returned string. `RenderTo` into a reusable writer makes no allocations for these scenarios; `TestRenderAllocs` asserts that. Run the library's own benchmarks with `go test -bench . -benchmem -run '^$'`.

## Comparison with raymond

[bench/COMPARISON.md](bench/COMPARISON.md) compares go-handlebars with raymond for developers choosing an engine. It covers:
- a feature matrix with each raymond entry cited against its source;
- the spec corpus subset that needs no JavaScript: 258 tests, of which go-handlebars passes 258 and raymond 183;
- the full benchmark table, with methodology;
- notes on when each engine fits.

The comparison lives in its own module, `bench/`, so this module's `go.mod` stays dependency-free.
