# go-handlebars and raymond compared

This compares go-handlebars with [raymond](https://github.com/aymerick/raymond), for Go developers choosing a Handlebars engine.

- **go-handlebars**: this repository. The library code is as of commit `e8f80b5`. It targets handlebars.js 4.7.9.
- **raymond**: `github.com/aymerick/raymond v2.0.2+incompatible`, its latest tag (2018-03-22). Its README says it has "the same features as handlebars.js `3.0`".

Raymond citations are `file:line` relative to the raymond v2.0.2 module root (`$(go env GOMODCACHE)/github.com/aymerick/raymond@v2.0.2+incompatible`). "Probe" names a test in `bench/probe_test.go` that shows the behavior. The probes are skipped by default; run them with `cd bench && RAYMOND_PROBES=1 go test -run TestProbe -v .`. `TestProbeLazyPartialRace` needs `-race` and then fails with the race report. `TestProbeRecursivePartial` kills the test process, so it has its own switch: `RAYMOND_PROBE_STACK=1 go test -run TestProbeRecursivePartial -v .`. The numbers below come from one such run on the benchmark machine (§3).

## 1. Feature matrix

| Feature | go-handlebars | raymond v2.0.2 | Notes |
| --- | --- | --- | --- |
| Blocks, inverse sections, else chains | yes | yes | raymond: `parser/parser.go:280` (block), `:322` (`{{^x}}`), `:365` (`{{else if}}` chain). Test "chained inverted sections" at `handlebars/blocks_test.go:105`. |
| Block params (`as \|a b\|`) | yes | yes, two names at most (value, then index or key) | raymond binds `BlockParams[0]` and `[1]` only (`eval.go:237-252`). Test "#each - each with block params" at `handlebars/builtins_test.go:202`. |
| `each` / `if` / `unless` / `with` / `lookup` / `log` | all six, with handlebars.js semantics | all six, plus `equal` (`helper.go:26-35`), with differences | raymond differences:<br>- `lookup` always returns a string (`helper.go:370-372`), so `{{#each (lookup . "xs")}}` renders nothing (probe: `TestProbeSemantics`).<br>- `log` takes exactly one string and writes it with the standard `log` package (`helper.go:364-367`). Two arguments give a "wrong number of arguments" error (`eval.go:601-603`, probe: `TestProbeSemantics`).<br>- `includeZero` only recognizes a Go `int` 0 (`helper.go:264-274`), not a `float64` from `encoding/json`.<br>- `each` over a map uses `MapKeys()` order, which is random (`helper.go:325-337`). |
| `@data` variables: `@root` `@index` `@key` `@first` `@last` | yes | yes, with one difference | raymond:<br>- Each iteration sets `@index` `@key` `@first` `@last` (`data_frame.go:34-43`).<br>- `@root` is at `eval.go:447-460`.<br>- Initial `@data` comes from `ExecWith(ctx, *DataFrame)` (`template.go:207`); helpers read it via `Options.Data` / `DataFrame` (`helper.go:169-189`).<br>- `@root` resolves against the root context before a `root` passed in `@data` (`eval.go:447-460`), so spec test "passed root values take priority" fails.<br>- No `@level` (README.md:1295). |
| Subexpressions | yes | yes | raymond: `eval.go:949-953`, `parser/parser.go:553`. |
| Hash arguments | yes | yes | raymond: `eval.go:986-998`. It drops a pair whose value is nil (`eval.go:992`). |
| Partials: static, dynamic, context, hash | all four; context plus hash is merged, as in handlebars.js 4 | static; dynamic `{{> (expr)}}` (`eval.go:867-872`); one context argument (`eval.go:700-702`); hash alone | raymond:<br>- A hash alone becomes the partial's whole context (`eval.go:704-707`).<br>- Context plus hash is an error: "Passing both context and named parameters to a partial is not allowed" (`eval.go:696-698`).<br>- The partial name `404/asdf?.bar` fails to parse (spec "Partials with complex path"). |
| Partial blocks (`{{#> p}}…{{/p}}`) and `@partial-block` | yes | no | raymond's lexer has no `{{#>` token (`lexer/lexer.go:60-82`). `{{#> p}}` gives the lexer error "Unexpected character in expression: '>'" (18 corpus tests, §2). |
| Inline partials (`{{#*inline}}`) | yes | no | `*` is not an identifier character (`lexer/lexer.go:53`). `{{#*inline}}` gives "Unexpected character in expression: '*'" (13 corpus tests, §2). |
| Decorators | built-in `inline` only; no public API to register custom decorators | no | Decorators are deprecated in handlebars.js 4. go-handlebars runs the corpus's decorator tests through an unexported hook. raymond has the same lexer limitation as inline partials. |
| Whitespace control `~` | yes | yes | raymond: `~?` in the open and close patterns (`lexer/lexer.go:64-82`), stripping in `parser/whitespace.go`. All 36 whitespace-control.json tests pass (§2). |
| Standalone lines | yes | yes | raymond: `parser/whitespace.go:171` (programs), `:244` (blocks). It indents standalone partials (`parser/whitespace.go:206`, `eval.go:730`). |
| Raw blocks `{{{{raw}}}}` | yes | yes | raymond: `lexer/lexer.go:60-63`, `parser/parser.go:225-277`. Its README says "raw block content is not passed as a parameter to helper" (README.md:1291). |
| HTML escaping set | ``& < > " ' ` =`` as handlebars.js (`&#x27;` `&#x60;` `&#x3D;`) | `& ' < > "`, with `'` as `&apos;` | raymond: `escape.go:22`, `escape.go:31-41`. `` ` `` and `=` are written unescaped. These two matter in unquoted HTML attribute values. Spec test "escaping expressions" fails. |
| `strict` / `assumeObjects` | yes | no | Listed as not implemented (README.md:1284-1285). `Parse(source string)` takes no options (`template.go:33`). |
| `knownHelpers` / `knownHelpersOnly` | yes | no | README.md:1280-1281. |
| `compat` | yes, opt-in | no option, but parent lookup is always on | raymond lists `compat` as not implemented (README.md:1279). Yet it resolves a name missing from the current context in the parent contexts (`eval.go:504-524`, mustache-style). So `{{#with a}}{{b}}{{/with}}` renders the outer `b` (probe: `TestProbeSemantics`), which handlebars.js does only with `compat`. |
| `preventIndent` | yes | no | README.md:1286. Indentation is always applied (`eval.go:730`). |
| `explicitPartialContext` | yes | no | raymond has no compile options at all (`template.go:33`). |
| `noEscape` | yes | no | README.md:1283. Both engines have `{{{x}}}`, `{{&x}}` and a `SafeString` type (raymond `string.go:9-12`). |
| `stringParams` / `trackIds` | yes (legacy; `HelperOptions.Legacy`) | no | README.md:1282, README.md:1287. |
| Ordered-object iteration | yes: `KeyedCollection` iterates in `Keys()` order; plain Go maps in sorted key order | no ordered map type | raymond: maps iterate in random order (`helper.go:325-337`, which notes the difference from JS); struct fields in declaration order (`helper.go:338-358`). Spec test GH-1021 gives different output across runs (§2). |
| Go structs, maps, slices | exported struct fields by exact Go name; string-keyed maps; slices and arrays (index, `length`); pointers followed | struct fields by name with the first letter upper-cased, or by a `handlebars:"name"` tag; methods; maps whose key type accepts a string; slice index | raymond:<br>- `strings.Title` on the name: `eval.go:321`.<br>- Tag: `eval.go:397-409`.<br>- Methods are called: `eval.go:355-371`.<br>- Maps: `eval.go:332-337`. Slices: `eval.go:338-341`.<br>- `fmt.Stringer` and `error` values print through their methods (`string.go:65-84`).<br>go-handlebars prints values the way handlebars.js does (`String(v)`) and ignores `Stringer`. |
| Function values in data | yes: values of type `Helper` are called with the current context as `this` | yes: any func-valued field or method with one result | raymond calls the function (`eval.go:345-349`, `eval.go:374-392`); one that does not have exactly one result is rejected with an error at Exec, "Helper function must return a string or a SafeString" (`helper.go:60-72`, converted by `template.go:233-234`; probe: `TestProbeSemantics`) and converts its arguments by reflection (`eval.go:581-645`). |
| Context cancellation | `RenderCtx`: `ctx.Err()` is checked before rendering and every 1024 ops | none | No `context.Context` anywhere in raymond's API (`template.go:193-224`). |
| Resource limits (size, depth, output, ops) | `Limits`: template size, parse depth, render depth, output size, ops; all on by default | none | raymond has no limit settings, and `evalPartial` has no depth counter (`eval.go:713-737`). Probes, each of which go-handlebars rejects with a `LimitError` at `DefaultLimits`:<br>- 50,000 nested blocks (600,000 bytes) parse in 209 ms (`TestProbeUnbounded`).<br>- Six nested `{{#each @root.x}}` over 10 items, a 158-byte template, render 2,000,000 bytes (10^6 iterations) in 446 ms (`TestProbeUnbounded`).<br>- A partial that includes itself recurses until the Go stack limit, which is `fatal error: stack overflow` and cannot be recovered (`TestProbeRecursivePartial`).<br>raymond lexes in a goroutine that sends each token on an unbuffered channel (`lexer/lexer.go:95-105`, `:156`). A lexer error ends that goroutine (`lexer/lexer.go:225-228`). A `Parse` that fails in the parser before the end of input leaves it blocked on its next send for good. `TestProbeGoroutineLeak`: 100 failed parses of `{{#a}}{{/b}} and {{c}}` leave 100 goroutines; `{{#> p}}x{{/p}}` and `{{foo` (lexer errors) leave 0. |
| Panic safety | the library returns errors instead of panicking, and is fuzzed for it; a panic in a user helper becomes an error wrapping `ErrHelperPanic` | panics that are `error` values become errors; runtime errors and other panic values are re-raised | raymond: `template.go:226-239` (Exec), `parser/parser.go:71-83` (Parse). A helper that panics with a string or a nil-map write crashes the caller unless the caller recovers (probe: `TestProbeHelperPanic`). Registering a duplicate helper or partial name panics (`template.go:112`, `template.go:133`, `helper.go:43`). No panics in the corpus subset (§2). |
| Concurrency of a compiled template | `Render`/`RenderTo`/`RenderCtx` are safe concurrently; registration is not | `Exec` is safe concurrently, except on the first use of a partial registered from source | raymond: each `Exec` gets a fresh evaluator (`template.go:217`), and the helper and partial maps sit behind an `RWMutex` (`template.go:20`). A partial registered with `RegisterPartial(name, source)` is parsed lazily, without synchronization (`partial.go:73-85`). `go test -race` reports a data race at `partial.go:75`/`:78` when the first `Exec`s run concurrently (probe: `TestProbeLazyPartialRace`, run with `-race`). `RegisterPartialTemplate` with a pre-parsed partial avoids it. |
| Dependencies | none (standard library only) | standard library only in non-test code | raymond v2.0.2 has no `go.mod` (hence `+incompatible`). Its tests import `gopkg.in/yaml.v2` (`mustache_test.go`), so `go mod tidy` records that as an indirect requirement in a consuming module (see `bench/go.mod`). |

### Other differences seen while testing

The example outputs below come from `TestProbeSemantics`.

- **Missing helper.** `{{missing 1}}` renders an empty string in raymond. handlebars.js and go-handlebars fail with `Missing helper: "missing"`. raymond has no `helperMissing` / `blockHelperMissing` hooks (README.md:1292-1293); go-handlebars lets you override both by registering helpers with those names.
- **Numbers.** raymond formats floats with `strconv.FormatFloat(f, 'f', -1, 64)` (`string.go:51`), so `1e21` renders as `1000000000000000000000` and `1e-7` as `0.0000001`. handlebars.js and go-handlebars give `1e+21` and `1e-7`.
- **Arrays.** raymond concatenates the elements (`string.go:39-42`): `[1,2]` renders `12`. handlebars.js and go-handlebars give `1,2`.
- **Channels and functions as values.** `{{x}}` with a `chan` value is an error in raymond ("Can't print value", `string.go:31-34`) and renders empty in go-handlebars.
- **Helper style.** raymond helpers are ordinary Go functions with typed parameters; arguments are converted by reflection (`eval.go:581-645`), with an optional trailing `*raymond.Options`. go-handlebars has one signature, `func(*HelperOptions) (any, error)`, and arguments arrive as `[]any`.
- **Registries.** raymond also has process-wide registries for helpers and partials (`helper.go:20-50`, `partial.go:15-44`), plus file helpers (`ParseFile`, `RegisterPartialFile(s)`). go-handlebars registers everything per template.

## 2. Empirical: the handlebars-spec corpus

handlebars-spec (commit `748917a`, handlebars.js v4.7.9) has 475 render tests. 258 of them need no JavaScript:
- no code object in `helpers`, `decorators`, `partials`, `data` or `runtimeOptions`;
- one more test (regressions.json) is excluded because it needs a sparse array, which only the library's internal harness can build.

`bench/spec_test.go` (`TestSpecSubset`) runs these 258 tests through both engines with the same rules:
- **Inputs.** Parse the template, register the string partials, and render with the decoded data and any `runtimeOptions.data`.
- **Pass criteria.** The output must equal `expected`. An `exception` test passes if the engine returns an error (for raymond, a panic also counts; panics are recovered and counted).
- **Data decoding.** go-handlebars gets JSON objects as an insertion-ordered `KeyedCollection`, the type it documents for JavaScript key order. raymond gets `encoding/json` maps, because it has no ordered-object type.
- **Repeat runs.** Each raymond test runs 20 times. Different results across runs count as a failure.
- **Compile options.** raymond has none. A test that sets an option to a non-default value is not run on raymond and counts as a failure, in the "unsupported compile option" category.

go-handlebars is stricter in its own harness (`spec_test.go`). There all 632 tests pass, and where a test names the expected error, the message must match too.

| File | Tests in subset | go-handlebars | raymond |
| --- | ---: | ---: | ---: |
| basic.json | 69 | 69 | 68 |
| blocks.json | 31 | 31 | 25 |
| builtins.json | 13 | 13 | 11 |
| data.json | 5 | 5 | 4 |
| helpers.json | 2 | 2 | 2 |
| partials.json | 56 | 56 | 21 |
| regressions.json | 25 | 25 | 16 |
| strict.json | 21 | 21 | 0 |
| whitespace-control.json | 36 | 36 | 36 |
| string-params.json, subexpressions.json, track-ids.json | 0 | – | – |
| **Total** | **258** | **258** | **183** |

raymond had 0 panics. Its 75 failures by category:

| Category | Tests | Examples |
| --- | ---: | --- |
| Unsupported compile option | 36 | Options set: `strict` 15, `compat` 7, `assumeObjects` 6, `data: false` 4, `explicitPartialContext` 2, `ignoreStandalone` 2, `knownHelpersOnly` 1, `preventIndent` 1. Two tests set two options each. |
| Partial blocks / `@partial-block` | 18 | partials.json "should render partial block as default"; regressions.json "GH-1341: 4.0.7 release breaks {{#if @partial-block}} usage". All fail with the lexer error on `>`. |
| Inline partials | 13 | partials.json "should define inline partials for template"; regressions.json "GH-1186: Support block params for existing programs". |
| Wrong output | 6 | All six are listed below. |
| Parse error | 1 | partials.json "Partials with complex path": `{{> 404/asdf?.bar}}`. |
| Different output across runs | 1 | regressions.json "GH-1021: Each empty string key". `each` over an object gave `Key: \nKey: name\nKey: value\n` in one run and `Key: name\nKey: value\nKey: \n` in another (Go map order). |

The six wrong outputs:

| Test | raymond | expected |
| --- | --- | --- |
| basic.json "escaping expressions", ``{{awesome}}`` with ``&"'`\<>`` | ``&amp;&quot;&apos;`\&lt;&gt;`` | ``&amp;&quot;&#x27;&#x60;\&lt;&gt;`` |
| builtins.json "if", `{{#if goodbye includeZero=true}}` with `goodbye: 0` | `cruel world!` (`includeZero` ignores a float64 0) | `GOODBYE cruel world!` |
| data.json "passed root values take priority", `{{@root.foo}}` with `@data` root `{foo: "hello"}` | `should not be used` | `hello` |
| regressions.json "GH-676: Using array in escaping mustache fails" | `12` | `1,2` |
| regressions.json "GH-731: zero context rendering", `{{#foo}} This is {{bar}} ~ {{/foo}}` with `foo: 0` | empty (the section treats 0 as false) | ` This is  ~ ` |
| regressions.json "GH-1319", `{{#each list}}{{#unless ./prop}}parent={{../value}} {{/unless}}{{/each}}` over `[null, "a"]` | `parent= parent=parent ` | `parent=parent parent=parent ` |

The GH-1319 result follows from raymond not pushing a nil context (`eval.go:254-257`). Inside a nil item, `../` then points one level too high.

Reproduce with `cd bench && go test -run TestSpecSubset -v .`. The test lists every raymond failure. It fails only if go-handlebars fails a test.

## 3. Benchmarks

Both engines render the library's own scenarios (`internal/benchdata`):
- **static**: about 6 KiB of HTML with two substitutions.
- **each**: `{{#each}}` over 1000 items with 3 fields.
- **partials**: 100 users through a standalone, indented `card` partial that includes `avatar` and `badge`.
- **escape**: 20 snippets of quote- and `&`-dense markup.

Before any timing, `compile()` in `bench/bench_test.go` checks that both engines produce byte-identical output for each scenario; `TestSameOutput` runs the same check. Two adjustments make that possible:
- **Escape data.** In the escape data, `'` `` ` `` `=` are replaced with `"` `<` `>`, characters that both engines escape the same way. This keeps the length and the number of escapable characters. Only this scenario's data has any of them.
- **raymond partials.** They are parsed up front and registered with `RegisterPartialTemplate`. Otherwise raymond would parse them at the first `Exec`, outside the Parse benchmark.

Command: `cd bench && go test -bench . -benchmem -count=5 -run '^$'`. Machine: Apple M4 Pro, `uname -m` arm64, `go1.25.10 darwin/arm64`, raymond v2.0.2+incompatible. `benchstat` was not installed, so each cell is the median of the 5 runs; the ns/op spread across runs was under 10% in every row. Render means go-handlebars `Template.Render` and raymond `Template.Exec`; both return a string.

| Benchmark | go-handlebars ns/op | raymond ns/op | raymond ÷ go-handlebars | go-handlebars B/op | raymond B/op | go-handlebars allocs/op | raymond allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Render/static | 671 | 2,369 | 3.5× | 6,148 | 19,206 | 1 | 28 |
| Render/each | 142,148 | 3,351,842 | 23.6× | 40,986 | 18,766,396 | 1 | 35,535 |
| Render/partials | 31,605 | 266,149 | 8.4× | 12,296 | 859,861 | 1 | 7,108 |
| Render/escape | 37,934 | 205,430 | 5.4× | 57,384 | 1,205,147 | 1 | 804 |
| Parse/static | 6,219 | 868,698 | 139.7× | 2,896 | 2,006 | 26 | 46 |
| Parse/each | 2,027 | 13,600 | 6.7× | 5,952 | 3,959 | 50 | 96 |
| Parse/partials | 4,288 | 33,258 | 7.8× | 11,360 | 9,126 | 113 | 216 |
| Parse/escape | 1,500 | 10,283 | 6.9× | 3,624 | 3,070 | 40 | 74 |

- **Render allocations.** go-handlebars' one allocation per Render is the returned string. Rendering into a reusable `io.Writer` with `RenderTo` makes 0 allocations for all four scenarios; `TestRenderAllocs` in the library asserts this.
- **Parse bytes.** raymond allocates fewer bytes per Parse in every scenario, but takes 6.7× to 140× as long. Its lexer runs in its own goroutine and passes each token over an unbuffered channel (`lexer/lexer.go:95-105`, `:156`). Parse/static is about 6 KiB of plain content.

## 4. When to choose which

**go-handlebars** fits when any of these applies:
- Your templates use handlebars.js 4 features: partial blocks, inline partials, `@partial-block`, or the compile options (`strict`, `assumeObjects`, `compat`, `knownHelpersOnly`, `preventIndent`, `explicitPartialContext`, `noEscape`).
- You need output byte-identical to handlebars.js, for example when templates are shared with a JavaScript front end. That covers the escaping set, number and array formatting, and object key order.
- Templates come from people you don't fully trust. go-handlebars has size, depth, output and op limits and `context` cancellation, and it is fuzzed against panics. raymond has no limits or cancellation, and a recursive partial crashes the process.
- Render throughput or allocations matter. See §3.

go-handlebars is new and has no adoption record yet. Struct fields must use their exact Go names, and helpers all have one untyped signature.

**raymond** has been available since 2015 (v1.0.0 on 2015-06-08, v2.0.2 on 2018-03-22). As of 2026-11-12:
- pkg.go.dev lists 322 importing packages (358 importing modules).
- Its GitHub repository is archived, so it is read-only and gets no further fixes.

It offers a Go-flavored API:
- typed helper parameters converted by reflection;
- lower-camel names, `handlebars:"…"` tags and methods on structs;
- `fmt.Stringer` output;
- global registries and file loaders.

It fits trusted templates written against the handlebars.js 3 feature set, where its differences from handlebars.js 4 output (escaping, `compat`-style parent lookup, map order, number formatting) are acceptable. An existing raymond codebase also has its helpers written in raymond's typed-function style, and moving them to go-handlebars means rewriting each one to `func(*HelperOptions) (any, error)`.
