# go-handlebars Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go Handlebars library that passes all 632 tests of `jbboehr/handlebars-spec` (the JSON export of the handlebars.js test suite) and is safe to run on untrusted templates: every parse and render is bounded by explicit limits (bytes, depth, operation count), and rendering is allocation-frugal (buffers, never string concatenation in hot paths).

**Architecture:** Three classic stages — hand-written lexer → recursive-descent parser producing an immutable AST → tree-walking renderer writing into a counting output buffer. A compiled `*Template` is immutable and safe for concurrent rendering. A `Limits` struct is threaded through both parse and render; every byte written, every nesting level, and every substitution/iteration is accounted against it.

**Tech Stack:** Go ≥ 1.22, standard library only (no third-party deps). Test corpus: `handlebars-spec/spec/*.json` (git submodule, already added). Reference implementation vendored at `handlebars-spec/handlebars.js/` (nested submodule, already initialized).

## Global Constraints

- **Zero third-party dependencies.** `go.mod` lists nothing but the module itself.
- Module path: `github.com/nikolaymatrosov/go-handlebars`, single package `handlebars` at repo root. Internal test tooling may live in subdirs.
- **Security invariants (apply to every task, not just the security phase):**
  - `Parse` rejects sources longer than `Limits.MaxTemplateSize` before scanning.
  - Parser tracks nesting depth (blocks, subexpressions, partial blocks); exceeding `Limits.MaxParseDepth` → `*LimitError`.
  - Renderer tracks runtime depth (block scopes + partial expansions combined); exceeding `Limits.MaxRenderDepth` → `*LimitError`. This is what stops recursive partials.
  - Every byte of output goes through one `outWriter`; it counts bytes and fails with `*LimitError` past `Limits.MaxOutputSize`.
  - Every substitution, helper call, and loop iteration increments an op counter; past `Limits.MaxOps` → `*LimitError`.
  - The renderer must never panic on any template/data combination (enforced by fuzzing in Phase 12). Internal invariant violations use `error` returns, not panics.
- **Performance invariants (apply to every task):**
  - No `+` string concatenation and no `fmt.Sprintf` in render paths. All output composition goes through `outWriter` methods. Numbers are formatted with `strconv.Append*` into a scratch buffer.
  - HTML escaping writes directly into the output buffer (scan-and-copy-segments, like `template.HTMLEscape`), never builds an intermediate escaped string.
  - Path expressions are resolved to `[]pathSegment` at parse time, not re-split per render.
  - Allocation audits come in Phase 13; until then just don't introduce gratuitous garbage.
- **Spec fidelity:** when behavior is in doubt, read the vendored reference: lexer grammar `handlebars-spec/handlebars.js/src/handlebars.l`, parser grammar `src/handlebars.yy`, AST printer `lib/handlebars/compiler/printer.js`, whitespace algorithm `lib/handlebars/compiler/whitespace-control.js`, standalone helpers `lib/handlebars/compiler/helpers.js`, builtins `lib/handlebars/helpers/*.js`, escaping/isEmpty `lib/handlebars/utils.js`, runtime semantics `lib/handlebars/runtime.js` and `lib/handlebars/internal/create-new-lookup-object.js`.
- Commit after every task (conventional commits, e.g. `feat: lexer passes tokenizer spec`). Never commit with failing gates for completed phases. No `Co-Authored-By: Claude` trailers.

## Spec Corpus Facts (measured, 2026-11-12)

| File | Tests | Kind |
| --- | --- | --- |
| tokenizer.json | 78 | token stream: `expected` = `[{name,text}...]` |
| parser.json | 79 | AST print: `expected` = printer string |
| basic.json | 90 | render |
| blocks.json | 39 | render (8 use decorators) |
| builtins.json | 42 | render |
| data.json | 22 | render |
| helpers.json | 87 | render |
| partials.json | 61 | render |
| regressions.json | 33 | render |
| strict.json | 22 | render |
| string-params.json | 8 | render, legacy `stringParams` |
| subexpressions.json | 18 | render (2 use `stringParams`) |
| track-ids.json | 17 | render, legacy `trackIds` (16 set the option) |
| whitespace-control.json | 36 | render |

Render-test fields: `template`, `data`, `expected` (578), `helpers` (188 tests), `partials` (69), `compileOptions` (86), `runtimeOptions` (27, always `{data: ...}`), `exception` (54), `message` (146), `decorators` (8, all in blocks.json).

`compileOptions` seen in corpus (support all): `strict` 17, `data:false` 16, `trackIds` 16, `stringParams` 10, `knownHelpersOnly` 9, `compat` 8 (3 blocks + 5 via partials), `assumeObjects` 6, `knownHelpers` 3, `ignoreStandalone` 2, `explicitPartialContext` 2, `preventIndent` 1.

**JS code in tests:** helper/decorator bodies appear as `{"!code": true, "javascript": "function (...) {...}"}` — 349 usages, **117 unique JS sources**. Additionally **36 function values are embedded inside `data`** (basic 18, helpers 7, builtins 6, data 2, regressions 2, subexpressions 1), so the value resolver must support Go functions found during lookup (handlebars calls a function-valued property with the current context as `this`).

**Key-order trap:** `{{#each}}` over an object iterates JS insertion order. Go maps are unordered and `encoding/json` loses key order. The harness decodes JSON objects into an ordered map type (Task 0), and the library iterates anything implementing `KeyedCollection` in `Keys()` order; plain Go maps are iterated in sorted-key order for determinism.

## Public API (locked — later tasks must match these names and signatures exactly)

```go
package handlebars

import ("context"; "io")

// Limits bounds resource usage for untrusted templates. Zero value fields
// fall back to the corresponding DefaultLimits field.
type Limits struct {
 MaxTemplateSize int // bytes of source accepted by Parse
 MaxParseDepth   int // nesting of blocks/subexpressions during parse
 MaxRenderDepth  int // runtime nesting: block scopes + partial expansions
 MaxOutputSize   int // total bytes the renderer may emit
 MaxOps          int // substitutions + helper calls + loop iterations
}

var DefaultLimits = Limits{
 MaxTemplateSize: 1 << 20,  // 1 MiB
 MaxParseDepth:   100,
 MaxRenderDepth:  100,
 MaxOutputSize:   8 << 20,  // 8 MiB
 MaxOps:          1_000_000,
}

// LimitError reports which limit was exceeded.
type LimitError struct {
 What string // "template size", "parse depth", "render depth", "output size", "ops"
 Max  int
}
func (e *LimitError) Error() string

// Error is a parse or runtime error with template position (1-based).
type Error struct {
 Line, Column int
 Message      string
}
func (e *Error) Error() string

// Options mirrors handlebars.js compile options plus Limits.
type Options struct {
 Limits                 Limits
 Strict                 bool
 AssumeObjects          bool
 Compat                 bool
 Data                   *bool // nil means true (compileOptions.data)
 KnownHelpers           map[string]bool
 KnownHelpersOnly       bool
 IgnoreStandalone       bool
 PreventIndent          bool
 ExplicitPartialContext bool
 NoEscape               bool
 StringParams           bool // legacy, Phase 11
 TrackIDs               bool // legacy, Phase 11
}

func Parse(src string) (*Template, error) // Parse(src) == ParseWithOptions(src, Options{})
func ParseWithOptions(src string, opts Options) (*Template, error)

type Template struct{ /* immutable AST + registries */ }
// Registration is not concurrency-safe; register everything before rendering.
func (t *Template) RegisterHelper(name string, h Helper)
func (t *Template) RegisterPartial(name, src string) error // parsed with t's Options
func (t *Template) RegisterPartialTemplate(name string, p *Template)
func (t *Template) Render(data any) (string, error)
func (t *Template) RenderTo(w io.Writer, data any) error
// RenderCtx checks ctx.Err() every 1024 ops; rendering is otherwise identical.
func (t *Template) RenderCtx(ctx context.Context, w io.Writer, data any) error
// RenderWithData is RenderTo plus initial @data values (runtimeOptions.data).
func (t *Template) RenderWithData(w io.Writer, data any, dataVars map[string]any) error

// SafeString is written to output without HTML escaping.
type SafeString string

// Helper implements a handlebars helper.
type Helper func(opts *HelperOptions) (any, error)

// HelperOptions carries evaluated arguments and block closures.
type HelperOptions struct {
 Name string         // helper name as written in the template
 Args []any           // evaluated positional args
 Hash map[string]any  // evaluated hash args
 // unexported: render state, AST node, block programs
}
func (o *HelperOptions) Ctx() any                        // current context ("this")
func (o *HelperOptions) IsBlock() bool                   // has a block program
func (o *HelperOptions) Fn() (string, error)             // render block with current ctx
func (o *HelperOptions) FnWith(ctx any) (string, error)  // render block with given ctx
func (o *HelperOptions) FnTo(w io.Writer, ctx any, blockParams []any, dataVars map[string]any) error // zero-copy path used by builtins
func (o *HelperOptions) Inverse() (string, error)
func (o *HelperOptions) InverseTo(w io.Writer, ctx any) error
func (o *HelperOptions) DataVar(name string) any         // @name lookup
func (o *HelperOptions) Lookup(path string) any          // path lookup from current ctx
func (o *HelperOptions) BlockParamNames() []string

// KeyedCollection lets callers supply insertion-ordered objects (used by
// {{#each}} and property lookup). The spec harness relies on it.
type KeyedCollection interface {
 Keys() []string
 Get(key string) (any, bool)
 Len() int
}
```

## File Structure

| File | Responsibility |
| --- | --- |
| `handlebars.go` | Public API above: Parse, Template, Options, Limits, errors, SafeString |
| `token.go` | Token kind enum (names identical to handlebars.js: `CONTENT`, `OPEN`, `ID`, ... derived from tokenizer.json), Token struct {Kind, Text (offsets into src), Line, Col} |
| `lexer.go` | Zero-alloc scanner: `lex(src string, limits Limits) ([]Token, error)` |
| `ast.go` | AST node types + `printAST(node) string` replicating `printer.js` |
| `parser.go` | Recursive descent over tokens → `*program`; depth limit; `Parse error on line N:` messages |
| `whitespace.go` | Tilde stripping + standalone-line algorithm (port of `whitespace-control.js`), honors `IgnoreStandalone` |
| `value.go` | Dynamic value ops: `lookupProperty`, truthiness/`isEmpty`, `jsString` number/value formatting, function-valued property calls, `KeyedCollection`, reflect-based struct/map/slice access |
| `escape.go` | `writeEscaped(dst *outWriter, s string)` HTML escaping (`& < > " ' \x60 =`), SafeString handling |
| `render.go` | `renderState` (context stack, data frames, depth, ops, limits), `outWriter` (byte counting), node evaluation |
| `helpers.go` | `Helper`, `HelperOptions` methods, helper registry, `helperMissing`/`blockHelperMissing` hooks |
| `builtins.go` | `each`, `if`, `unless`, `with`, `lookup`, `log` |
| `partials.go` | Partial registry/resolution, dynamic partials, partial blocks, inline partials, `@partial-block`, indentation handling |
| `spec_test.go` | Harness: loads `handlebars-spec/spec/*.json`, ordered-map JSON decode, runs all tests, reports pass/fail/skip |
| `spec_funcs_test.go` | Table: SHA-1[JS source](:12) → Go `Helper`/value, 117 entries filled across phases |
| `limits_test.go` | Hand-written adversarial tests for Limits (spec doesn't cover them) |
| `fuzz_test.go` | `FuzzParse`, `FuzzRender` |
| `bench_test.go` | Benchmarks + allocation assertions (Phase 13) |

---

### Task 0: Repo bootstrap + spec harness ✅

**Files:**

- Create: `go.mod`, `spec_test.go`, `spec_funcs_test.go`, stub `handlebars.go`
- Already present: `spec/` (mustache, keep as reference), `handlebars-spec/` submodules

**Interfaces:**

- Produces: `specTest` struct, `omap` ordered map (implements `KeyedCollection`), `specFuncs map[string]any` registry, test entry points `TestSpecTokenizer`, `TestSpecParser`, `TestSpecRender`.
- The harness is the project's test suite; later tasks only add entries to `specFuncs` and flip expected pass counts.

- [x] **Step 1: `go mod init github.com/nikolaymatrosov/go-handlebars`** (adjust path if the repo gets a different remote later).

- [x] **Step 2: Write the harness.** Core pieces that must exist exactly:

```go
// spec_test.go
type specTest struct {
 Description string          `json:"description"`
 It          string          `json:"it"`
 Template    string          `json:"template"`
 Data        json.RawMessage `json:"data"`
 Expected    *string         `json:"expected"`
 Helpers     json.RawMessage `json:"helpers"`
 Partials    map[string]string `json:"partials"`
 CompileOptions map[string]any `json:"compileOptions"`
 RuntimeOptions map[string]any `json:"runtimeOptions"`
 Exception   bool            `json:"exception"`
 Message     string          `json:"message"`
 Decorators  json.RawMessage `json:"decorators"`
 Number      string          `json:"number"`
}

// omap preserves JSON object key order; implements KeyedCollection.
type omap struct { keys []string; m map[string]any }
func (o *omap) Keys() []string { return o.keys }
func (o *omap) Get(k string) (any, bool) { v, ok := o.m[k]; return v, ok }
func (o *omap) Len() int { return len(o.keys) }

// decodeOrdered parses JSON with json.Decoder token-by-token so every
// object becomes *omap, every array []any, numbers float64 via
// dec.UseNumber() converted (keep int64 when integral is NOT needed:
// use float64 everywhere, JS has only doubles).
func decodeOrdered(raw []byte) (any, error)

// resolveCode walks a decoded value; each *omap with key "!code" is
// replaced by specFuncs[sha1hex(js)[:12]]. Missing hash => return
// errSkip{hash, js} so the test is reported SKIP with a ready-to-paste
// stub line.
func resolveCode(v any) (any, error)
```

Runner logic per render test: `decodeOrdered` data → `resolveCode` → build `Options` from `compileOptions` → `ParseWithOptions` → register partials (recursively via `RegisterPartial`) and helpers (from resolved `Helpers` map) → `RenderWithData` into `strings.Builder` → compare with `*Expected`. If `Exception`: require non-nil error; if `Message` non-empty, require `msgMatch(err, test.Message)` where `msgMatch` consults a normalization table `msgAliases map[string]string` (JS message → acceptable Go message or prefix) and otherwise does exact compare. Filters: env `HBSPEC_FILE` (file basename), `HBSPEC_IT` (substring of `description/it`). `HBSPEC_STUBS=1` prints missing `specFuncs` stubs. Final line per file: `basic.json: 0/90 pass, 0 fail, 90 skip` style summary; a test for a not-yet-implemented area counts FAIL (not skip) — only missing `specFuncs` entries produce SKIP.

- [x] **Step 3: Stub `handlebars.go`** with the full locked API above, all bodies `return nil, errors.New("not implemented")` (or no-ops), so the harness compiles.

- [x] **Step 4: Run `go test ./... 2>&1 | tail -20`** — expect compilation OK, all 632 tests failing/skipping with per-file summaries printed.

- [x] **Step 5: Commit** `feat: spec harness, locked public API, limits types`.

---

### Task 1: Lexer → tokenizer.json 78/78 ✅

**Files:**

- Create: `token.go`, `lexer.go`

**Interfaces:**

- Produces: `type tokenKind uint8` with `String()` returning handlebars.js names; `type token struct { kind tokenKind; text string; line, col int }`; `func lex(src string, limits Limits) ([]token, error)`.
- Consumes: `Limits` from Task 0.

- [x] **Step 1: Derive the exact token-name set from the corpus** (don't guess):
  `python3 -c "import json;print(sorted({tok['name'] for t in json.load(open('handlebars-spec/spec/tokenizer.json')) for tok in t['expected']}))"`

- [x] **Step 2: Port the scanner from `handlebars-spec/handlebars.js/src/handlebars.l`.** It is a small flex grammar (~60 rules) with two states (`mu` inside mustaches, `raw` inside `{{{{raw}}}}` blocks). Hand-write it as a state machine over `src` by index; token `text` is a subslice of `src` (no copies) except where the grammar transforms text (e.g. `\\{{` escapes, quoted strings strip quotes and unescape `\"`). Enforce `MaxTemplateSize` at the top of `lex`.

- [x] **Step 3: Wire `TestSpecTokenizer`** to call `lex` and compare `[]{name,text}`.

- [x] **Step 4: Run `HBSPEC_FILE=tokenizer.json go test -run TestSpecTokenizer -v | tail -5`** — iterate until **78/78 pass**.

- [x] **Step 5: Commit** `feat: lexer passes tokenizer spec (78/78)`.

---

### Task 2: AST + parser + whitespace control → parser.json 79/79 ✅

**Files:**

- Create: `ast.go`, `parser.go`, `whitespace.go`

**Interfaces:**

- Produces (internal, used by renderer tasks):

```go
type program struct { body []node; blockParams []string }
type node interface{ pos() (line, col int) }
// node kinds: contentNode{text string}, mustacheNode{expr, escaped bool, strip},
// blockNode{expr, program *program, inverse *program, strip, isDecorator, isRawBlock bool, chained bool},
// partialNode{name expr or pathExpr, ctxParam expr, hash, indent string, strip, isBlock bool, program *program},
// commentNode, pathExpr{data bool, depth int, parts []string, original string},
// subExpr{path pathExpr, params []expr, hash []hashPair},
// literals: strLit, numLit(float64), boolLit, undefinedLit, nullLit
func parse(src string, opts Options) (*program, error)
func printAST(p *program) string // byte-identical to printer.js output
```

- [x] **Step 1: Define AST mirroring `handlebars.yy` productions** (MustacheStatement, BlockStatement, PartialStatement, PartialBlockStatement, DecoratorBlock, ContentStatement, CommentStatement, SubExpression, PathExpression, Hash, literals, `blockParams` as `as |a b|`).

- [x] **Step 2: Port `printer.js` as `printAST`** — this is the oracle for parser.json. Examples from corpus: `{{foo}}` → `"{{ PATH:foo [] }}\n"`, `{{123}}` → `"{{ NUMBER{123} [] }}\n"`. Match it byte-for-byte including depth padding.

- [x] **Step 3: Write the recursive-descent parser** over `lex` output, porting the grammar semantics in `handlebars.yy` plus the AST-building helpers in `lib/handlebars/compiler/helpers.js` (`prepareBlock`, `preparePath` — path depth `../` handling, `this`/`[seg]` parts, "Invalid path" errors), with a `depth int` guard against `MaxParseDepth` (increment on block open, subexpression, partial block). Error text format: `Parse error on line N: ...` (see `parser-prefix.js` / spec `message` fields for exact strings; add `msgAliases` entries when exact Jison "Expecting ..." lists are impractical — but prefer exact).

- [x] **Step 4: Port `whitespace-control.js`** as an AST pass run inside `parse`: tilde `~` strips (`strip.open/close`), standalone-line detection/removal for blocks, comments, partials; `Options.IgnoreStandalone` disables the standalone part. Note `preventIndent`/partial indent capture happens here (`partialNode.indent`).

- [x] **Step 5: Run `HBSPEC_FILE=parser.json go test -run TestSpecParser | tail -3`** — iterate to **79/79**. Tokenizer must stay 78/78.

- [x] **Step 6: Commit** `feat: parser + whitespace control pass parser spec (79/79)`.

---

### Task 3: Core renderer → basic.json 90/90 ✅ (90/90 after Task 6)

**Files:**

- Create: `render.go`, `value.go`, `escape.go`; fill in `handlebars.go` bodies
- Modify: `spec_funcs_test.go` (add the ~18 data-function entries basic.json needs)

**Interfaces:**

- Produces (internal):

```go
type outWriter struct { w io.Writer; buf []byte; written, max int; scratch [32]byte }
// methods: WriteString, WriteByte, Write, writeEscaped(s string), flush() error
// every method returns *LimitError once written > max

type renderState struct {
 t *Template; out *outWriter
 ctxStack []any       // index 0 = root; lookups walk ../ by slicing
 dataRoot *dataFrame  // linked frames: {parent *dataFrame; vars map[string]any}
 depth, ops int
 limits Limits
 ctx context.Context  // nil outside RenderCtx
}
func (s *renderState) step() error // ops++, check MaxOps, every 1024: ctx.Err()

// value.go
func lookupProperty(parent any, name string, assumeObjects bool) (any, bool)
// supports *omap/KeyedCollection, map[string]any (and reflect maps),
// []any (and reflect slices) by numeric index, struct fields via reflect,
// returns (nil,false) for missing — never panics.
func isEmpty(v any) bool   // JS: !value && value !== 0, plus empty array/slice
func isTruthy(v any) bool  // !isEmpty for section logic (see utils.js)
func jsString(v any) string            // nil/undefined => "", numbers via jsNumString
func writeValue(out *outWriter, v any, escape bool) error // SafeString bypass
func jsNumString(f float64) string     // JS Number#toString: integral => no ".0",
// shortest round-trip otherwise; exponent for >=1e21 and <1e-6; NaN/Infinity names
```

- [x] **Step 1: Implement `escape.go`.** Escape set from `utils.js`: `& < > " ' \x60 =` → `&amp; &lt; &gt; &quot; &#x27; &#x60; &#x3D;`. `writeEscaped` scans for the next escapable byte, bulk-writes the clean segment, writes the entity, repeats. Fast path: no escapable bytes → one `WriteString`.

- [x] **Step 2: Implement `value.go`** per signatures above. Function-valued properties: if lookup yields a value of type `Helper` or `func(...) ...` registered via specFuncs, the mustache evaluator calls it with the current context (mirror `runtime.js` `lambda` handling: zero-arg Go funcs `func() any` and context-taking `func(ctx any) any` both supported; the harness's specFuncs use these two shapes plus `Helper`).

- [x] **Step 3: Implement the evaluator in `render.go`** for: content nodes, escaped/unescaped mustaches with path expressions (`foo.bar`, `../x`, `this`, `[literal segs]`, `@data` paths deferred to Task 7 — return nil for now), literal mustaches (`{{123}}`), raw blocks content, comments (emit nothing). `Render`/`RenderTo`/`RenderCtx`/`RenderWithData` per the locked API; `Render` uses an internal `[]byte` buffer sized `min(len(src)*2, 4096)` then one string copy out.

- [x] **Step 4: Fill basic.json's missing `specFuncs`** — run `HBSPEC_STUBS=1 HBSPEC_FILE=basic.json go test -run TestSpecRender` and port each printed JS function to Go (most are one-liners like `function() { return 'bar'; }`).

- [x] **Step 5: Iterate `HBSPEC_FILE=basic.json go test -run TestSpecRender | tail -3` to 90/90, 0 skip.** Expect the long tail to be `jsNumString` edge cases and escaping.

- [x] **Step 6: Commit** `feat: core renderer passes basic spec (90/90)`.

---

### Task 4: Blocks → blocks.json 31/39 (all except 8 decorator tests) ✅ (39/39 after Task 10)

**Files:**

- Modify: `render.go` (block evaluation), `value.go`
- Modify: `spec_funcs_test.go`

**Interfaces:**

- Consumes: `blockNode`, `isEmpty`, `renderState`.
- Produces: `evalBlock` handling: truthy section (push context), array iteration, inverted `{{^x}}`, `{{else}}`, chained `{{else if ...}}` (the `chained` flag renders the inverse's single block without standalone re-strip), block params `as |v k|`, `../` context walking, `this` sections. Depth guard: `s.depth++` on every block/program entry, check `MaxRenderDepth`; `s.step()` per iteration.

- [x] **Step 1: Implement plain-path block semantics** mirroring `blockHelperMissing` in `lib/handlebars/helpers/block-helper-missing.js`: value truthy non-array → render program with value as context (unless value is `true` → current context); array → iterate (each element pushed, ops counted); falsy/empty → inverse.

- [x] **Step 2: Block params:** a program with `blockParams` resolves those names from a per-frame slice before normal context lookup (see `runtime.js` blockParams chain).

- [x] **Step 3: Compat mode** (`Options.Compat`, 3 tests here): failed lookup retries walking up the context stack depth-first.

- [x] **Step 4: Run `HBSPEC_FILE=blocks.json go test -run TestSpecRender | tail -3`** — target **31 pass / 8 skip (decorators, Task 10) / 0 fail**. basic.json must stay green.

- [x] **Step 5: Commit** `feat: block rendering (blocks spec 31/39, decorators pending)`.

---

### Task 5: Helpers API → helpers.json green except stringParams/trackIds/knownHelpers edge set ✅ (87/87 after Task 6)

**Files:**

- Create: `helpers.go`
- Modify: `render.go` (mustache/block dispatch order), `spec_funcs_test.go` (bulk of the 117 entries lands here)

**Interfaces:**

- Produces: the locked `Helper` + `HelperOptions` API; registry `map[string]Helper` on `Template`; dispatch rules ported from `runtime.js`/compiler: a mustache/block whose path is a registered helper name (or `knownHelpers`) is a helper call; `helperMissing` hook fires for `{{foo 1}}` with args and no helper; `blockHelperMissing` for block fallthrough; hash args evaluated into `map[string]any`; subexpression params deferred to Task 8 (parser already builds them; evaluator for `subExpr` params lands there — plain params here).
- `HelperOptions.Fn/FnWith/Inverse` render the block program into a pooled `[]byte` buffer and return `string`; `FnTo/InverseTo` write straight into the caller's writer (builtins use these; byte/op accounting flows through the same `outWriter` either way — a helper-returned string is written (escaped unless SafeString) through `writeValue`, so `MaxOutputSize` still holds).

- [x] **Step 1: Implement dispatch + `HelperOptions`** per above. Helper return values: `string`, `SafeString`, numbers, bool, nil, any — written via `writeValue`. Errors from helpers abort rendering and surface from `Render*`.

- [x] **Step 2: `KnownHelpers`/`KnownHelpersOnly`** (9+3 tests): with `KnownHelpersOnly`, non-known paths are always context lookups, known names always helper calls (match `compiler.js` semantics).

- [x] **Step 3: Port helper specFuncs.** `HBSPEC_STUBS=1 HBSPEC_FILE=helpers.json ...` and port JS bodies. Convention for the table: value shapes `Helper`, `func() any`, `func(ctx any) any`; key = `sha1hex(jsSource)[:12]`. Port faithfully including `options.fn(this)` → `o.Fn()`, `options.inverse()` → `o.Inverse()`, `options.hash`, `arguments.length` checks → `len(o.Args)`.

- [x] **Step 4: Run helpers.json** — target: **0 fail; skips only for tests whose compileOptions include stringParams/trackIds** (count them with `python3 - <<…`; expect ≈0–5, they mostly live in their own files). blocks/basic stay green.

- [x] **Step 5: Commit** `feat: helpers API + registry (helpers spec green mod legacy opts)`.

---

### Task 6: Builtins + data frames → builtins.json 42/42 ✅

**Files:**

- Create: `builtins.go`
- Modify: `render.go` (dataFrame plumbing), `spec_funcs_test.go`

**Interfaces:**

- Consumes: `HelperOptions.FnTo(w, ctx, blockParams, dataVars)`.
- Produces: default registry (every new Template gets it; user registrations shadow): `each` (arrays, KeyedCollection in Keys() order, plain maps in sorted-key order, sets `@index @key @first @last`, block params `|value key|`), `if`/`unless` (`includeZero` hash), `with` (context push + block param + else), `lookup` (safe, uses `lookupProperty`), `log` (writes to a `Logger func(level string, args ...any)` field on Template, default no-op; `level` hash arg). Port each from `lib/handlebars/helpers/<name>.js`.
- `dataFrame`: created by `each` iterations (`createFrame` semantics: copy-on-write child of parent frame; `@../index` reaches parent frame via `pathExpr.depth` on data paths).

- [x] **Step 1: Implement dataFrame + `@` path evaluation** in `render.go` (pathExpr with `data=true`: resolve from current frame, `depth` walks parent frames).

- [x] **Step 2: Implement the six builtins** with `FnTo` (no intermediate strings), `s.step()` per iteration.

- [x] **Step 3: Port remaining specFuncs for builtins.json; run to 42/42.** Prior files stay green.

- [x] **Step 4: Commit** `feat: builtin helpers + data frames (builtins 42/42)`.

---

### Task 7: @data semantics → data.json 22/22 ✅ (22/22 after Task 9)

**Files:**

- Modify: `render.go`, `handlebars.go` (`RenderWithData` initial frame, `Options.Data=false` disables frame creation), `spec_funcs_test.go`

- [x] **Step 1:** `runtimeOptions.data` (27 tests corpus-wide) seeds the root frame — harness already passes it via `RenderWithData`. Implement `@root` (auto-set to root context), nested overrides, `Options.Data == false` → data tracking off (16 tests; `{{@foo}}` then resolves nil / helpers see no frame).

- [x] **Step 2:** Run data.json to **22/22**; re-run full suite for regressions.

- [x] **Step 3: Commit** `feat: @data variables (data 22/22)`.

---

### Task 8: Subexpressions → subexpressions.json 16/18 (2 stringParams skips) ✅ (18/18 after Task 11)

**Files:**

- Modify: `render.go`/`helpers.go` (evaluate `subExpr` params: inner helper call whose return value becomes the arg; nested depth counts toward `MaxRenderDepth` via `s.depth`), `spec_funcs_test.go`

- [x] **Step 1:** Implement `evalExpr` for `subExpr` (always a helper call — unknown name → `helperMissing`, matching JS behavior incl. its error `"<name> doesn't match <other>"`-style messages in corpus).
- [x] **Step 2:** Run to **16 pass / 2 skip (stringParams) / 0 fail**. Commit `feat: subexpressions (16/18)`.

---

### Task 9: Partials → partials.json 61/61 ✅

**Files:**

- Create: `partials.go`
- Modify: `render.go`, `spec_funcs_test.go`

**Interfaces:**

- Produces: resolution order (inline partials from enclosing blocks → registered partials); dynamic partials `{{> (expr)}}`; custom context `{{> p ctx}}` and hash → context merge (hash-only → hash as context extension, `ExplicitPartialContext` forces hash/explicit only); partial blocks `{{#> p}}fallback{{/p}}` with `@partial-block`; inline partials `{{#*inline "name"}}` (store program in render-state scope chain — this is the one decorator the core needs; generic decorators are Task 10); failover error `The partial <name> could not be found`; indentation: each line of partial output indented by `partialNode.indent` unless `PreventIndent` — implement in `outWriter` as an active-indent mode (scan written chunks for `\n`, inject indent; no string rebuild).
- **Security:** every partial expansion does `s.depth++` + check — this is the recursive-partial bomb guard; add `s.step()` too.

- [x] **Step 1:** Implement static/dynamic/context/hash resolution + missing-partial error.
- [x] **Step 2:** Partial blocks + `@partial-block` (nested: each block pushes a frame var).
- [x] **Step 3:** Inline partials incl. shadowing rules; `Compat` partial tests (4).
- [x] **Step 4:** Indentation + `PreventIndent` (1 test) + standalone partial indent capture from Task 2.
- [x] **Step 5:** Run partials.json to **61/61**; full-suite regression check. Commit `feat: partials (61/61)`.

---

### Task 10: Whitespace render tests, strict mode, regressions, decorators ✅

**Files:**

- Modify: `render.go` (strict), `parser.go` (decorator AST already parsed in Task 2), `helpers.go` (decorator exec), `spec_funcs_test.go`

- [x] **Step 1: whitespace-control.json → 36/36** (mostly already green from Task 2's AST strip flags; fix evaluator's handling of `strip` on content boundaries).
- [x] **Step 2: strict.json → 22/22.** `Strict`: missing *property on existing-path lookups* and missing helpers → `*Error` with messages like `"<name>" not defined in <context>` (copy exact strings from the corpus `message` fields). `AssumeObjects`: skip existence checks on intermediate segments (nil deref must still return a clean error, never panic).
- [x] **Step 3: decorators (blocks.json 8 tests) → blocks.json 39/39.** Minimal decorator machinery mirroring `runtime.js`: decorators run before their block's program, can replace/wrap the program's helper fn and mutate a `props` map. Internal `type decorator func(prog *boundProgram, props map[string]any, o *HelperOptions) (*boundProgram, error)`; harness registers test decorators from `specFuncs`. Built-in: `inline` (already used by Task 9 — refactor it onto this mechanism if that is a simplification, otherwise leave it; `ponytail:` note the choice).
- [x] **Step 4: regressions.json → 33/33.** Grab-bag; expect fixes across value.go/render.go. Add `msgAliases` entries as needed.
- [x] **Step 5: Commit** `feat: strict mode, decorators, regressions (whitespace 36, strict 22, blocks 39, regressions 33)`.

---

### Task 11: Legacy options → string-params.json 8/8, track-ids.json 17/17, remaining skips → 0 ✅ (632/632)

**Files:**

- Modify: `helpers.go`, `render.go`, `spec_funcs_test.go`

- [x] **Step 1: `StringParams`** (10 tests): args passed to helpers as path *strings* instead of resolved values; `options.types`/`options.hashTypes` arrays exposed on `HelperOptions` as `Types []string` / `HashTypes map[string]string` (add these two exported fields — they are additive, allowed). Mirror `compiler.js` stringParams mode.
- [x] **Step 2: `TrackIDs`** (17 tests): `options.ids`/`hashIds` → `IDs []string` / `HashIDs map[string]string` fields; ids are contextual source paths (see `track-ids.json` expectations and `javascript-compiler.js` `trackIds` handling).
- [x] **Step 3: Full sweep:** `go test ./... | tail -20` → **632/632 pass, 0 fail, 0 skip**. Fix stragglers.
- [x] **Step 4: Commit** `feat: stringParams + trackIds; full handlebars-spec green (632/632)`.

---

### Task 12: Adversarial security tests + fuzzing ✅

**Files:**

- Create: `limits_test.go`, `fuzz_test.go`

- [x] **Step 1: Write the limit tests** (the spec corpus does not cover limits — these are mandatory hand-written tests):

```go
func TestLimitParseDepth(t *testing.T) {
 src := strings.Repeat("{{#if x}}", 2000) + strings.Repeat("{{/if}}", 2000)
 _, err := Parse(src)
 var le *LimitError
 if !errors.As(err, &le) || le.What != "parse depth" { t.Fatalf("got %v", err) }
}

func TestLimitRecursivePartials(t *testing.T) {
 tpl, _ := Parse("{{> a}}")
 tpl.RegisterPartial("a", "{{> b}}")
 tpl.RegisterPartial("b", "{{> a}}")
 _, err := tpl.Render(nil)
 var le *LimitError
 if !errors.As(err, &le) || le.What != "render depth" { t.Fatalf("got %v", err) }
}

func TestLimitOutputSize(t *testing.T) { // billion-laughs via nested each
 tpl, _ := ParseWithOptions("{{#each a}}{{#each ../a}}xxxxxxxxxx{{/each}}{{/each}}",
  Options{Limits: Limits{MaxOutputSize: 1 << 16}})
 big := make([]any, 1000)
 _, err := tpl.Render(map[string]any{"a": big})
 var le *LimitError
 if !errors.As(err, &le) { t.Fatalf("got %v", err) }
}

func TestLimitOps(t *testing.T) { /* same shape, Limits{MaxOps: 1000}, expect What=="ops" */ }
func TestLimitTemplateSize(t *testing.T) { /* Parse(strings.Repeat("x", DefaultLimits.MaxTemplateSize+1)) */ }
func TestRenderCtxCancel(t *testing.T) { /* canceled context -> ctx.Err() surfaces mid-render */ }
func TestLimitErrorsNotSwallowed(t *testing.T) { /* LimitError inside helper Fn() must abort the whole render, errors.As from Render */ }
```

- [x] **Step 2: Fuzzing.**

```go
func FuzzParse(f *testing.F) {
 // seed with ~20 templates sampled from the spec corpus
 f.Fuzz(func(t *testing.T, src string) {
  tpl, err := ParseWithOptions(src, Options{Limits: Limits{MaxTemplateSize: 1 << 16, MaxParseDepth: 64}})
  if err != nil { return }
  var sb strings.Builder // must not panic, must respect limits
  _ = tpl.RenderTo(&sb, map[string]any{"a": []any{1.0, "x", nil}})
 })
}
```

Run `go test -fuzz=FuzzParse -fuzztime=120s` and fix every panic/hang found (commit repro seeds into `testdata/fuzz/`). Then a `FuzzRender` variant fuzzing the data side (JSON-decode the fuzz bytes into data for a fixed set of spec templates).

- [x] **Step 3: Commit** `test: adversarial limit tests + fuzzing`.

---

### Task 13: Performance — benchmarks and allocation audit ✅

**Files:**

- Create: `bench_test.go`; targeted edits across `render.go`/`value.go`/`escape.go`

- [x] **Step 1: Benchmarks** with `b.ReportAllocs()`: (a) static-heavy template, (b) `{{#each}}` over 1000 items with 3 fields, (c) partial-heavy, (d) escape-heavy. Baseline numbers recorded in the commit message.
- [x] **Step 2: Audit with `-benchmem` + profiles.** Required outcomes: zero allocations per render for (a) when using `RenderTo` with a reusable writer (pool `renderState` + `outWriter` in a `sync.Pool`); no per-iteration allocations in (b) beyond unavoidable `any` boxing of looked-up values; `jsNumString`/numbers formatted via `strconv.AppendFloat` into `outWriter.scratch`.
- [x] **Step 3: `testing.AllocsPerRun` assertions** in `bench_test.go` for case (a) (`== 0`) and an escape-only write (`== 0`), so regressions fail CI, not just benchmarks.
- [x] **Step 4: README.md**: API example, security model (Limits table with defaults and what attack each stops), spec-compliance statement (632/632, corpus commit hash), concurrency contract. Written for library users.
- [x] **Step 5: Commit** `perf: pooled render state, zero-alloc static render + README`.

---

## Self-Review Notes (already applied)

- Handlebars cannot pass mustache/spec (delimiters, lambdas) — the `spec/` submodule stays as reference only; no task targets it. User confirmed handlebars-spec is the target.
- All 11 observed `compileOptions` map to `Options` fields; `exception` tests (54) and `message` matching policy live in the Task 0 harness; the 36 data-embedded functions are covered by `resolveCode` walking `data` (Task 0) + lambda-call semantics (Task 3).
- Gate arithmetic: 78+79 (Tasks 1–2) + 90 (T3) + 39 (T4+T10) + 87 (T5+T11) + 42 (T6) + 22 (T7) + 18 (T8+T11) + 61 (T9) + 36+22+33 (T10) + 8+17 (T11) = 632.
- Decorator tests and `inline` partials share machinery; Task 9 implements `inline` standalone, Task 10 may refactor — flagged, not mandated.

---

# v0.2.0: template inspection and policy API

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the API that lets a consumer vet an untrusted template before it renders: an expression budget (`Limits.MaxExpressions`), `errors.Is` sentinels for every limit, a logic-less mode (`Options.NoBlocks`), and `Template.Variables()`, which lists the root-context paths a template reads. Then release v0.2.0.

**Architecture:** `MaxExpressions` is counted by the lexer over expression-opening tokens, so lexing stops at the first expression over the limit and nothing past it is tokenized or parsed. `NoBlocks` is one check in `parser.statement`. `Variables()` is a single recursive pass over the existing AST, in a new `variables.go`. The render path does not change.

**Tech Stack:** Go 1.22, standard library only.

## v0.2.0 Global Constraints

- Standard library only. Go 1.22.
- No panics on any template, data or option combination. The fuzzers must keep passing.
- With default `Options`, the spec suite still passes 632/632. Every new option is off or permissive by default, except the new `MaxExpressions` default (100,000).
- The render hot path gets no new allocations or work. R1 and R3 touch only the lexer and the parser. `Variables` is a separate method that is not called during a render.
- `Template` stays safe for concurrent rendering.
- Conventional commits. No `Co-Authored-By` trailers.
- Stage files by explicit path. Never `git add -A` or `git add .`: the requirements document in the repo root (`2026-11-17-go-handlebars-missing-api-design.md`) is untracked on purpose and must stay out of git.
- Work on the branch `release/v0.2.0`. When a task's review is clean, tick its steps and add ` ✅` to its heading in this file (`docs(roadmap): mark task N done`).

## v0.2.0 Decisions (agreed 2026-11-17)

These settle what the requirements leave open. Tasks implement them exactly.

1. **`MaxExpressions` is counted in the lexer**, not in `parser.call`. One expression is counted per opening token: `OPEN` (which includes `{{&` and `{{*`), `OPEN_UNESCAPED`, `OPEN_BLOCK` (which includes `{{#*`), `OPEN_INVERSE`, `OPEN_INVERSE_CHAIN`, `OPEN_RAW_BLOCK`, `OPEN_PARTIAL`, `OPEN_PARTIAL_BLOCK` and `OPEN_SEXPR`. In a valid template each of these starts exactly one `parser.call`, so the counts match the requirements table. Lexing stops at the first opener over the limit. Before this change, a 1 MiB source spent 163 MB in the lexer alone, before parsing started. Arguments and hash pairs are not counted, so a single expression with many of them is bounded by `MaxTemplateSize` alone. The README says so.
2. **Limit 0 means the default.** So for a row whose count is 1, "count − 1 fails" cannot be expressed. For every row with count n ≥ 1, `TestLimitExpressions` also checks the row repeated twice: it parses at 2n and fails at 2n−1.
3. **`Variables`: a scoped or multi-segment callee is a variable, even with arguments.** `{{this.fmt price}}` gives `[[fmt] [price]]` and `{{a.b c}}` gives `[[a b] [c]]`. In Strict mode both read the callee from the data and fail when it is missing. Only a bare name (one segment, not `this`/`./`/`../`/`@`) can be a helper reference.
4. **Decorators** (`{{* d x}}`, `{{#*inline n}}…`). The callee and arguments are not variables, but `@root.x` in their arguments counts. The body of a decorator block is not root scope.
5. **A `@root.x` callee counts**, e.g. `{{@root.f x}}` gives `[[f] [x]]`, because it reads data.
6. **`KnownHelpers` makes a name a helper only when its value is `true`.** A builtin name stays a helper even when `KnownHelpers` maps it to `false`.
7. **Source order for `{{^x}}A{{else}}B{{/x}}`.** The parser swaps `program` and `inverse` for inverted blocks, so `blockNode` gets an `inverted` flag and the walk visits `inverse` first when it is set.
8. **Root scope is only the top level and builtin `if`/`unless` bodies**, both branches and `{{else if}}` chains included. A `{{#with}}`'s `{{else}}` is not root scope.
9. **`NoBlocks` error text:** `Message: "Block statements are not allowed"`, so `Error()` is `Block statements are not allowed - L:C`.
10. **Fuzzing:** `fuzzOptions` bit 11 sets `NoBlocks` and bit 12 sets `MaxExpressions: 16`, both used by `FuzzParse`. `FuzzRender` keeps its fixed templates, which contain blocks.
11. **Release** is full: fast-forward `main`, annotated tag `v0.2.0` with the release notes, push `main` and the tag, then `gh release create`. The notes cover the new `MaxExpressions` default and the new unexported `Error` field, which breaks unkeyed `handlebars.Error{…}` literals outside the package.

## v0.2.0 File Structure

| File | Change |
| --- | --- |
| `handlebars.go` | `Limits.MaxExpressions` and its default; limit sentinels and `(*LimitError).Is`; `Options.NoBlocks`, `ErrBlocksNotAllowed`, `Error.cause` and `(*Error).Unwrap` |
| `lexer.go` | `exprOpen` token set; expression counter in the mu state |
| `parser.go` | `blockStart` set and the `NoBlocks` check in `statement`; `prepareBlock` sets `blockNode.inverted` |
| `ast.go` | `blockNode.inverted` |
| `helpers.go` | `isBuiltin(name)` extracted from `(*Options).known` |
| `variables.go` (new) | `Template.Variables` and its AST walker |
| `limits_test.go` | `TestLimitExpressions`, `TestLimitSentinels` |
| `parser_test.go` | `TestNoBlocks`; `TestParseLinear` raises `MaxExpressions` |
| `variables_test.go` (new) | `TestVariables` |
| `render_test.go` | `TestRenderParallel` calls `Variables` concurrently |
| `fuzz_test.go` | new option bits; `FuzzParse` checks `Variables` |
| `example_test.go`, `README.md` | docs |

---

### Task 14: `Limits.MaxExpressions`, counted by the lexer ✅

**Files:**
- Modify: `handlebars.go` (`Limits`, `DefaultLimits`, `withDefaults`, `LimitError.What` comment)
- Modify: `lexer.go` (`lex`)
- Modify: `parser_test.go` (`TestParseLinear`)
- Test: `limits_test.go`

**Interfaces:**
- Produces: `Limits.MaxExpressions int`; `DefaultLimits.MaxExpressions == 100_000`; parse failures `&LimitError{What: "expressions", Max: n}`.

- [x] **Step 1: Write the failing test.** Append to `limits_test.go`:

```go
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
```

- [x] **Step 2: Run it to verify it fails.**

Run: `go test -run TestLimitExpressions .`
Expected: FAIL to compile with `unknown field MaxExpressions in struct literal of type Limits`.

- [x] **Step 3: Add the limit.** In `handlebars.go`, add the field at the end of `Limits` (after `MaxOps`):

```go
	// MaxExpressions bounds how many expressions Parse accepts: every
	// mustache, block (including each `{{else x}}` link), subexpression,
	// partial, partial block, raw block and decorator counts as one.
	// Parsing stops at the first expression over the limit, before the
	// rest of the source is tokenized.
	MaxExpressions int
```

In `DefaultLimits`, add `MaxExpressions: 100_000,` after `MaxOps`. In `withDefaults`, before `return l`, add:

```go
	if l.MaxExpressions == 0 {
		l.MaxExpressions = d.MaxExpressions
	}
```

In `LimitError`, change the `What` comment to `// "template size", "parse depth", "render depth", "output size", "ops", "expressions"`.

- [x] **Step 4: Count in the lexer.** In `lexer.go`, above `func lex`, add:

```go
// exprOpen are the tokens that open an expression. In a valid template each
// starts one parser.call: a mustache or decorator, a block or `{{else x}}`
// link, a raw block, a partial or partial block, or a subexpression. lex
// counts them against Limits.MaxExpressions.
var exprOpen = setOf(tOPEN, tOPEN_UNESCAPED, tOPEN_BLOCK, tOPEN_INVERSE, tOPEN_INVERSE_CHAIN,
	tOPEN_RAW_BLOCK, tOPEN_PARTIAL, tOPEN_PARTIAL_BLOCK, tOPEN_SEXPR)
```

Change the doc comment of `lex` to:

```go
// lex scans src into tokens, ending with tEOF. On a lexical error it returns
// the tokens before it (no tEOF) with the error, so the parser can report
// earlier errors first, as the lazy JS lexer does. A *LimitError (template
// size, expressions) comes with no tokens: lexing stops at the first
// expression over Limits.MaxExpressions.
```

In the `var (...)` block of `lex`, add `exprs int // expression-opening tokens so far`. In the `case stMu:` branch, at the start of `default:` (before the inner `switch k`), add:

```go
				if exprOpen.has(k) {
					if exprs++; exprs > limits.MaxExpressions {
						return nil, &LimitError{What: "expressions", Max: limits.MaxExpressions}
					}
				}
```

`parse` already returns a lexer `*LimitError` before parsing, so `parser.go` needs no change.

- [x] **Step 5: Keep `TestParseLinear` meaningful.** It builds about 105,000 expressions to time the parser. In `parser_test.go`, change its options to `Options{Limits: Limits{MaxParseDepth: n, MaxExpressions: n}}`.

- [x] **Step 6: Run the tests.**

Run: `go test -run 'TestLimitExpressions|TestParseLinear|TestLex' -v . 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: every listed test is `--- PASS`.
Run: `go test ./...`
Expected: `ok` (spec suite 632/632).

- [x] **Step 7: Commit.**

```bash
gofmt -l . ; go vet ./...
git add handlebars.go lexer.go limits_test.go parser_test.go
git commit -m "feat: Limits.MaxExpressions, counted by the lexer"
```

---

### Task 15: Limit sentinels ✅

**Files:**
- Modify: `handlebars.go`
- Test: `limits_test.go`

**Interfaces:**
- Consumes: `Limits.MaxExpressions` and the `"expressions"` `LimitError` (Task 14).
- Produces: `ErrTemplateSize`, `ErrParseDepth`, `ErrRenderDepth`, `ErrOutputSize`, `ErrOps`, `ErrExpressions` (`error`), and `func (e *LimitError) Is(target error) bool`.

- [x] **Step 1: Write the failing test.** Append to `limits_test.go`:

```go
// Each limit's LimitError matches its own sentinel and no other, directly
// and wrapped, and errors.As still finds the *LimitError.
func TestLimitSentinels(t *testing.T) {
	render := func(src string, l Limits, partials map[string]string) error {
		tpl, err := ParseWithOptions(src, Options{Limits: l})
		if err != nil {
			return err
		}
		for name, p := range partials {
			if err := tpl.RegisterPartial(name, p); err != nil {
				return err
			}
		}
		_, err = tpl.Render(nil)
		return err
	}
	sentinels := []error{ErrTemplateSize, ErrParseDepth, ErrRenderDepth, ErrOutputSize, ErrOps, ErrExpressions}
	cases := []struct {
		what string
		want error
		err  error
	}{
		{"template size", ErrTemplateSize, render("xx", Limits{MaxTemplateSize: 1}, nil)},
		{"parse depth", ErrParseDepth, render("{{#if a}}{{#if a}}{{/if}}{{/if}}", Limits{MaxParseDepth: 1}, nil)},
		{"render depth", ErrRenderDepth, render("{{> a}}", Limits{MaxRenderDepth: 5}, map[string]string{"a": "{{> a}}"})},
		{"output size", ErrOutputSize, render("xxxx", Limits{MaxOutputSize: 2}, nil)},
		{"ops", ErrOps, render("{{a}}{{a}}{{a}}", Limits{MaxOps: 1}, nil)},
		{"expressions", ErrExpressions, render("{{a}}{{a}}", Limits{MaxExpressions: 1}, nil)},
	}
	for _, c := range cases {
		var le *LimitError
		if !errors.As(c.err, &le) || le.What != c.what {
			t.Errorf("%s: got %v, want a %s LimitError", c.what, c.err, c.what)
			continue
		}
		for _, err := range []error{c.err, fmt.Errorf("render: %w", c.err)} {
			for _, s := range sentinels {
				if got := errors.Is(err, s); got != (s == c.want) {
					t.Errorf("%s: errors.Is(%q, %q) = %v", c.what, err, s, got)
				}
			}
			if le = nil; !errors.As(err, &le) || le.What != c.what {
				t.Errorf("%s: errors.As(%q) lost the LimitError", c.what, err)
			}
		}
	}
}
```

- [x] **Step 2: Run it to verify it fails.**

Run: `go test -run TestLimitSentinels .`
Expected: FAIL to compile with `undefined: ErrTemplateSize`.

- [x] **Step 3: Implement.** In `handlebars.go`, add `"errors"` to the imports and, after the `LimitError` type, add:

```go
// Sentinels for errors.Is, one per limit: a *LimitError matches the one for
// its What, also when wrapped.
var (
	ErrTemplateSize = errors.New("handlebars: template size limit exceeded")
	ErrParseDepth   = errors.New("handlebars: parse depth limit exceeded")
	ErrRenderDepth  = errors.New("handlebars: render depth limit exceeded")
	ErrOutputSize   = errors.New("handlebars: output size limit exceeded")
	ErrOps          = errors.New("handlebars: ops limit exceeded")
	ErrExpressions  = errors.New("handlebars: expressions limit exceeded")
)

// limitSentinels maps LimitError.What to its sentinel.
var limitSentinels = map[string]error{
	"template size": ErrTemplateSize,
	"parse depth":   ErrParseDepth,
	"render depth":  ErrRenderDepth,
	"output size":   ErrOutputSize,
	"ops":           ErrOps,
	"expressions":   ErrExpressions,
}

// Is reports whether target is the sentinel for e.What, e.g. ErrOps for an
// "ops" LimitError.
func (e *LimitError) Is(target error) bool {
	s, ok := limitSentinels[e.What]
	return ok && s == target
}
```

- [x] **Step 4: Run the tests.**

Run: `go test -run TestLimitSentinels -v . 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: `--- PASS: TestLimitSentinels`.
Run: `go test ./...`
Expected: `ok`.

- [x] **Step 5: Commit.**

```bash
gofmt -l . ; go vet ./...
git add handlebars.go limits_test.go
git commit -m "feat: errors.Is sentinels for every limit"
```

---

### Task 16: `Options.NoBlocks` and `(*Error).Unwrap` ✅

**Files:**
- Modify: `handlebars.go` (`Error`, `Options`, new sentinel)
- Modify: `parser.go` (`blockStart`, `statement`)
- Test: `parser_test.go`

**Interfaces:**
- Produces: `Options.NoBlocks bool`; `ErrBlocksNotAllowed error`; `func (e *Error) Unwrap() error`; the unexported field `Error.cause error`.

- [x] **Step 1: Write the failing test.** Add `"errors"` and `"fmt"` to the imports of `parser_test.go` and append:

```go
// NoBlocks rejects every block statement at its opening tag; everything
// else parses and renders as without it.
func TestNoBlocks(t *testing.T) {
	noBlocks := Options{NoBlocks: true}
	rejected := []struct {
		src       string
		line, col int
	}{
		{"{{#if a}}x{{/if}}", 1, 0},
		{"a\n  {{^a}}x{{/a}}", 2, 2},
		{"{{x}}{{#if a}}y{{else}}z{{/if}}", 1, 5},
		{"{{#if a}}y{{else if b}}z{{/if}}", 1, 0},
		{"{{{{raw}}}} {{x}} {{{{/raw}}}}", 1, 0},
		{"{{#> p}}x{{/p}}", 1, 0},
		{`{{#*inline "n"}}{{x}}{{/inline}}`, 1, 0},
		{"{{~#each a~}}{{/each}}", 1, 0},
		{"ё{{> p}}{{#with a}}{{#if b}}{{/if}}{{/with}}", 1, 8}, // first block; JS column in UTF-16 units
		{"{{#if a}}{{/if}}{{!-- never closed", 1, 0},           // parsing stops at the block
	}
	for _, c := range rejected {
		_, err := ParseWithOptions(c.src, noBlocks)
		var he *Error
		if !errors.Is(err, ErrBlocksNotAllowed) || !errors.As(err, &he) || he.Line != c.line || he.Column != c.col {
			t.Errorf("%q: got %v, want ErrBlocksNotAllowed at %d:%d", c.src, err, c.line, c.col)
		}
		if _, err := Parse(c.src); errors.Is(err, ErrBlocksNotAllowed) {
			t.Errorf("%q without NoBlocks: %v", c.src, err)
		}
	}
	if _, err := ParseWithOptions("{{#if a}}{{/if}}", noBlocks); err == nil || err.Error() != "Block statements are not allowed - 1:0" {
		t.Errorf("error text: %v", err)
	}

	// RegisterPartial parses with the template's Options.
	tpl, err := ParseWithOptions("{{> p}}", noBlocks)
	if err != nil {
		t.Fatal(err)
	}
	if err := tpl.RegisterPartial("p", "{{#if a}}{{/if}}"); !errors.Is(err, ErrBlocksNotAllowed) {
		t.Errorf("RegisterPartial: got %v, want ErrBlocksNotAllowed", err)
	}

	allowed := []string{
		"{{x}} {{{x}}} {{&x}}",
		"{{fmt x y k=x}}",
		"{{fmt (fmt x) k=(fmt y)}}",
		`{{> p}} {{> p y}} {{> p k=x}} {{> (lookup . "pn")}}`,
		"a {{! c }} {{!-- c --}} b",
		"{{* deco}}{{x}}",
		`\{{x}} {{x}}`,
	}
	data := map[string]any{"x": "<b>", "y": 2, "pn": "p"}
	for _, src := range allowed {
		var outs [2]string
		for i, opts := range []Options{{}, noBlocks} {
			tpl, err := ParseWithOptions(src, opts)
			if err != nil {
				t.Fatalf("%q (NoBlocks %v): %v", src, opts.NoBlocks, err)
			}
			tpl.RegisterHelper("fmt", func(o *HelperOptions) (any, error) { return fmt.Sprint(o.Args, o.Hash), nil })
			tpl.registerDecorator("deco", func(map[string]any, *HelperOptions) error { return nil })
			if err := tpl.RegisterPartial("p", "[{{this}}{{k}}]"); err != nil {
				t.Fatal(err)
			}
			if outs[i], err = tpl.Render(data); err != nil {
				t.Fatalf("%q (NoBlocks %v): %v", src, opts.NoBlocks, err)
			}
		}
		if outs[0] != outs[1] {
			t.Errorf("%q: NoBlocks renders %q, without it %q", src, outs[1], outs[0])
		}
	}

	// Other errors have no cause.
	_, syntaxErr := Parse("{{foo}")
	strict, err := ParseWithOptions("{{a}}", Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	_, renderErr := strict.Render(map[string]any{})
	for _, err := range []error{syntaxErr, renderErr} {
		var he *Error
		if !errors.As(err, &he) || errors.Unwrap(err) != nil || errors.Is(err, ErrBlocksNotAllowed) {
			t.Errorf("%v: want an *Error with no cause", err)
		}
	}
}
```

- [x] **Step 2: Run it to verify it fails.**

Run: `go test -run TestNoBlocks .`
Expected: FAIL to compile with `unknown field NoBlocks in struct literal of type Options`.

- [x] **Step 3: Extend `Error` and `Options`.** In `handlebars.go`, replace the `Error` type and its method with:

```go
// Error is a parse or runtime error with template position, like a
// handlebars.js Exception with a loc: Line is 1-based, Column is the 0-based
// JS column (UTF-16 units). Line 0 means no position.
type Error struct {
	Line, Column int
	Message      string
	cause        error // what Unwrap returns: ErrBlocksNotAllowed for a NoBlocks rejection, else nil
}

func (e *Error) Error() string {
	if e.Line > 0 { // mirrors the handlebars.js Exception: "msg - line:col"
		return fmt.Sprintf("%s - %d:%d", e.Message, e.Line, e.Column)
	}
	return e.Message
}

// Unwrap returns ErrBlocksNotAllowed for the error Options.NoBlocks makes
// Parse return, and nil for every other Error.
func (e *Error) Unwrap() error { return e.cause }

// ErrBlocksNotAllowed is what the *Error for a block statement under
// Options.NoBlocks wraps.
var ErrBlocksNotAllowed = errors.New("handlebars: block statements are not allowed")
```

In `Options`, after `TrackIDs bool`, add:

```go
	// NoBlocks makes Parse reject every block statement, leaving the
	// logic-less subset: substitutions, helper calls, subexpressions,
	// partials, comments and decorators. The error is an *Error at the
	// first block's opening tag that wraps ErrBlocksNotAllowed.
	// RegisterPartial parses with it too; a partial passed to
	// RegisterPartialTemplate keeps its own Options, so parse those with
	// NoBlocks as well.
	NoBlocks bool
```

- [x] **Step 4: Reject blocks in the parser.** In `parser.go`, add to the `var (...)` block that holds `stmtStart`:

```go
	// blockStart opens a block statement: {{#x}} and {{#*x}}, {{^x}},
	// {{{{x}}}} and {{#> p}}. Options.NoBlocks rejects these.
	blockStart = setOf(tOPEN_BLOCK, tOPEN_INVERSE, tOPEN_RAW_BLOCK, tOPEN_PARTIAL_BLOCK)
```

At the top of `statement`, right after `t := p.peek()`, add:

```go
	if p.opts.NoBlocks && blockStart.has(t.kind) {
		return nil, &Error{Line: t.line, Column: t.col, Message: "Block statements are not allowed", cause: ErrBlocksNotAllowed}
	}
```

The error is raised at a token before the final EOF, so `parse` returns it ahead of any later lexical error.

- [x] **Step 5: Run the tests.**

Run: `go test -run TestNoBlocks -v . 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: `--- PASS: TestNoBlocks`.
Run: `go test ./...`
Expected: `ok`.

- [x] **Step 6: Commit.**

```bash
gofmt -l . ; go vet ./...
git add handlebars.go parser.go parser_test.go
git commit -m "feat: Options.NoBlocks rejects block statements at parse time"
```

---

### Task 17: `Template.Variables()` ✅

**Files:**
- Create: `variables.go`
- Create: `variables_test.go`
- Modify: `ast.go` (`blockNode`)
- Modify: `parser.go` (`prepareBlock`)
- Modify: `helpers.go` (`known`)
- Modify: `render_test.go` (`TestRenderParallel`)

**Interfaces:**
- Produces: `func (t *Template) Variables() [][]string`; `blockNode.inverted bool`; `isBuiltin(name string) bool`.

- [x] **Step 1: Write the failing test.** Create `variables_test.go`:

```go
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
		{src: "{{* deco x @root.r}}", want: [][]string{{"r"}}},           // 4
		{src: `{{#*inline "p"}}{{@root.z}}{{/inline}}`, want: [][]string{{"z"}}},
		{src: "{{@root.f x}}", want: [][]string{{"f"}, {"x"}}}, // 5
		{src: "{{k}}", known: map[string]bool{"k": true}, want: nil},
		{src: "{{k}}", known: map[string]bool{"k": false}, want: [][]string{{"k"}}}, // 6
		{src: "{{if}}", known: map[string]bool{"if": false}, want: nil},             // 6: still a builtin
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
```

- [x] **Step 2: Run it to verify it fails.**

Run: `go test -run TestVariables .`
Expected: FAIL to compile with `tpl.Variables undefined`.

- [x] **Step 3: Record inverted blocks.** In `ast.go`, add a field to `blockNode` after `decorator bool`:

```go
	inverted                            bool       // {{^x}}: the parser swapped program and inverse, so inverse comes first in the source
```

Run `gofmt -w ast.go` to realign the field block. In `parser.go` `prepareBlock`, add `inverted: inverted,` to the `&blockNode{...}` literal.

- [x] **Step 4: Share the builtin list.** In `helpers.go`, replace `known` with:

```go
// known is compiler.js options.knownHelpers[name]: the builtins, overridden
// by Options.KnownHelpers.
func (o *Options) known(name string) bool {
	if v, ok := o.KnownHelpers[name]; ok {
		return v
	}
	return isBuiltin(name)
}

// isBuiltin reports whether name is a builtin helper or hook.
func isBuiltin(name string) bool {
	switch name {
	case "helperMissing", "blockHelperMissing", "each", "if", "unless", "with", "log", "lookup":
		return true
	}
	return false
}
```

- [x] **Step 5: Implement `Variables`.** Create `variables.go`:

```go
package handlebars

import (
	"slices"
	"strconv"
)

// Variables returns the paths into the root context that the template reads
// and that can be resolved statically, each as its segments, deduplicated, in
// order of first appearance. Call it after registering helpers: a bare name
// that matches a helper is a helper call, not a variable.
//
// Paths count where they render against the root context: the top level
// and, recursively, both branches and {{else if}} chains of the builtin if
// and unless, unless a helper of that name is registered. The bodies of
// other blocks, inline partials and partial blocks are skipped, except that
// @root.x is reported as x wherever it appears. A callee is a helper, not a
// variable, only when it is a bare name (one segment, no this, ./ or @) and
// the call has arguments or a hash, is a subexpression, or names a builtin,
// a KnownHelpers entry set to true or a helper registered on t. Literals,
// static partial names, other @data, block params, ../ paths, this, .,
// decorator names and arguments (but for @root.x) and the contents of
// registered partials are not reported: call Variables on those partials.
//
// Each inner slice is a fresh copy. Variables takes time linear in the
// template and is not bounded by Limits. It is safe to call concurrently
// with renders, but not while helpers or partials are being registered.
func (t *Template) Variables() [][]string {
	v := varWalker{t: t, seen: map[string]bool{}}
	v.program(t.prog, true)
	return v.out
}

// varWalker collects Template.Variables in one pass over the AST.
type varWalker struct {
	t    *Template
	seen map[string]bool // keys of the paths in out
	out  [][]string
	key  []byte // scratch for building seen keys
}

// program walks p's statements; root means they render against the root context.
func (v *varWalker) program(p *program, root bool) {
	if p == nil {
		return
	}
	for _, n := range p.body {
		switch n := n.(type) {
		case *mustacheNode: // a decorator's arguments only count for @root
			v.call(&n.call, root && !n.decorator, false)
		case *blockNode:
			v.call(&n.call, root && !n.decorator, false)
			inner := root && !n.decorator && v.ifBlock(n.path)
			first, second := n.program, n.inverse
			if n.inverted {
				first, second = second, first
			}
			v.program(first, inner)
			v.program(second, inner)
		case *partialNode:
			if s, ok := n.name.(*subExpr); ok {
				v.call(&s.call, root, true)
			}
			v.args(n.params, n.hash, root)
			v.program(n.program, false)
		}
	}
}

// call walks c's callee, unless it names a helper, then its arguments; sub
// means c is a subexpression.
func (v *varWalker) call(c *call, root, sub bool) {
	if p, ok := c.path.(*pathExpr); ok && !v.helperRef(p, c, sub) {
		v.path(p, root)
	}
	v.args(c.params, c.hash, root)
}

func (v *varWalker) args(params []node, hash []hashPair, root bool) {
	for _, n := range params {
		v.expr(n, root)
	}
	for _, h := range hash {
		v.expr(h.value, root)
	}
}

func (v *varWalker) expr(n node, root bool) {
	switch n := n.(type) {
	case *pathExpr:
		v.path(n, root)
	case *subExpr:
		v.call(&n.call, root, true)
	}
}

// helperRef reports whether callee p of c names a helper, not a variable.
func (v *varWalker) helperRef(p *pathExpr, c *call, sub bool) bool {
	if !bareName(p) {
		return false
	}
	name := p.parts[0]
	_, registered := v.t.helpers[name]
	return sub || len(c.params) > 0 || c.hash != nil || isBuiltin(name) || v.t.opts.KnownHelpers[name] || registered
}

// ifBlock reports whether a block calling path renders its bodies against
// the current context: the builtin if or unless, not replaced by a helper.
func (v *varWalker) ifBlock(path node) bool {
	p, ok := path.(*pathExpr)
	if !ok || !bareName(p) || p.bp || p.parts[0] != "if" && p.parts[0] != "unless" {
		return false
	}
	_, registered := v.t.helpers[p.parts[0]]
	return !registered
}

// bareName reports whether p is one plain segment: no @, ../, this or ./.
func bareName(p *pathExpr) bool {
	return !p.data && !p.scoped && p.depth == 0 && len(p.parts) == 1
}

// path records p if Variables reports it: @root.x anywhere, and in root
// scope a path into the context itself.
func (v *varWalker) path(p *pathExpr, root bool) {
	switch {
	case p.data:
		if p.depth == 0 && len(p.parts) > 1 && p.parts[0] == "root" {
			v.add(p.parts[1:])
		}
	case root && p.depth == 0 && !p.bp && len(p.parts) > 0:
		v.add(p.parts)
	}
}

// add appends a copy of parts to out unless an equal path is there already.
func (v *varWalker) add(parts []string) {
	v.key = v.key[:0]
	for _, s := range parts { // length-prefixed: a segment may hold any byte
		v.key = strconv.AppendInt(v.key, int64(len(s)), 10)
		v.key = append(v.key, ':')
		v.key = append(v.key, s...)
	}
	if !v.seen[string(v.key)] {
		v.seen[string(v.key)] = true
		v.out = append(v.out, slices.Clone(parts))
	}
}
```

- [x] **Step 6: Check concurrency under the race detector.** In `render_test.go` `TestRenderParallel`, inside the goroutine's `for i := 0; i < 10; i++` loop, after `buf.Reset()`, add:

```go
					_ = tpl.Variables() // reads the AST while other goroutines render
```

- [x] **Step 7: Run the tests.**

Run: `go test -run 'TestVariables|TestRenderParallel' -race -v . 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: both `--- PASS`.
Run: `go test ./...`
Expected: `ok`.

- [x] **Step 8: Commit.**

```bash
gofmt -l . ; go vet ./...
git add variables.go variables_test.go ast.go parser.go helpers.go render_test.go
git commit -m "feat: Template.Variables lists the root-context paths a template reads"
```

---

### Task 18: Fuzz the new options and `Variables` ✅

**Files:**
- Modify: `fuzz_test.go`

**Interfaces:**
- Consumes: `Options.NoBlocks`, `Limits.MaxExpressions`, `Template.Variables` (Tasks 14, 16, 17).

- [x] **Step 1: New option bits.** In `fuzzOptions`, add `NoBlocks: b(11)` to the `Options` literal and, before `return o`:

```go
	if b(12) {
		o.Limits.MaxExpressions = 16
	}
```

- [x] **Step 2: Seed both settings.** In `FuzzParse`, replace `f.Add(src, uint16(0))` with:

```go
		f.Add(src, uint16(0))
		f.Add(src, uint16(1<<11|1<<12)) // NoBlocks, MaxExpressions 16
```

- [x] **Step 3: Check `Variables` on every parsed template.** In `FuzzParse`, after the `if err != nil { return }` that follows `checkErr(t, "parse", err)`, add:

```go
		seen := map[string]bool{}
		for _, p := range tpl.Variables() {
			k := fmt.Sprintf("%q", p)
			if len(p) == 0 || seen[k] {
				t.Fatalf("Variables() = %q: empty or duplicate path", tpl.Variables())
			}
			seen[k] = true
		}
```

Add `"fmt"` to the imports. In the comment at the top of the file, after "Parse and Render never panic", add "(nor does Variables, which returns no empty or duplicate path)".

- [x] **Step 4: Run the fuzzers.**

Run: `go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 90s .`
Expected: `PASS` and no new files under `testdata/fuzz/`.
Run: `go test -run '^$' -fuzz '^FuzzRender$' -fuzztime 30s .`
Expected: `PASS`.
If a fuzzer fails, fix the library bug it found (superpowers:systematic-debugging), keep the generated `testdata/fuzz/...` seed, and add it in the commit.

- [x] **Step 5: Run the suite.**

Run: `go test ./...`
Expected: `ok`.

- [x] **Step 6: Commit.**

```bash
gofmt -l . ; go vet ./...
git add fuzz_test.go   # plus any testdata/fuzz seed by its path
git commit -m "test: fuzz NoBlocks, MaxExpressions and Variables"
```

---

### Task 19: Docs: README and `Example_untrusted` ✅

**Files:**
- Modify: `example_test.go` (`Example_untrusted`)
- Modify: `README.md`

**Interfaces:**
- Consumes: all of Tasks 14–17.

- [x] **Step 1: Rewrite `Example_untrusted`.** In `example_test.go`, replace the function and its comment with:

```go
// Templates from untrusted sources: allow only the logic-less subset,
// tighten the limits you care about (zero fields keep their DefaultLimits
// value), check the data a template needs before rendering it, and render
// under a deadline as well. Here the output limit stops the render.
func Example_untrusted() {
	opts := handlebars.Options{
		NoBlocks: true,
		Limits:   handlebars.Limits{MaxExpressions: 100, MaxOutputSize: 1 << 10},
	}
	for _, src := range []string{"{{#each rows}}{{.}}{{/each}}", strings.Repeat("{{x}}", 101)} {
		_, err := handlebars.ParseWithOptions(src, opts)
		switch {
		case errors.Is(err, handlebars.ErrBlocksNotAllowed):
			fmt.Println("unsupported:", err)
		case errors.Is(err, handlebars.ErrExpressions):
			fmt.Println("too complex:", err)
		}
	}

	tpl, err := handlebars.ParseWithOptions("Hi {{name}}! {{repeat motto times=n}}", opts)
	if err != nil {
		panic(err)
	}
	tpl.RegisterHelper("repeat", func(o *handlebars.HelperOptions) (any, error) {
		n, _ := o.Hash["times"].(int)
		return strings.Repeat(fmt.Sprint(o.Args[0]), n), nil
	})
	fmt.Println("needs:", tpl.Variables())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = tpl.RenderCtx(ctx, io.Discard, map[string]any{"name": "Ann", "motto": "0123456789", "n": 200})

	var le *handlebars.LimitError
	if errors.Is(err, handlebars.ErrOutputSize) && errors.As(err, &le) {
		fmt.Println(le.What, "limit exceeded, max", le.Max)
	}
	// Output:
	// unsupported: Block statements are not allowed - 1:0
	// too complex: handlebars: expressions limit exceeded (max 100)
	// needs: [[name] [motto] [n]]
	// output size limit exceeded, max 1024
}
```

Run: `go test -run Example -v . 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: `--- PASS: Example` and `--- PASS: Example_untrusted`.

- [x] **Step 2: Measure parse memory.** Create a scratch file `zz_mem_test.go` (do not commit it):

```go
package handlebars

import (
	"runtime"
	"strings"
	"testing"
)

func TestZZParseMem(t *testing.T) {
	for _, c := range []struct {
		name string
		src  string
		max  int
	}{
		{"100k {{a}}", strings.Repeat("{{a}}", 100_000), 0},
		{"1 MiB {{a}}, over the default", strings.Repeat("{{a}}", 209_715), 0},
		{"1 MiB {{a}}, MaxExpressions 100", strings.Repeat("{{a}}", 209_715), 100},
		{"1 MiB {{a b b ...}}", "{{a" + strings.Repeat(" b", 524_000) + "}}", 0},
		{"1 MiB {{a k=b ...}}", "{{a" + strings.Repeat(" k=b", 262_000) + "}}", 0},
		{"64 KiB {{a k=b ...}}", "{{a" + strings.Repeat(" k=b", 16_380) + "}}", 0},
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		tpl, err := ParseWithOptions(c.src, Options{Limits: Limits{MaxExpressions: c.max}})
		runtime.ReadMemStats(&after)
		total := after.TotalAlloc - before.TotalAlloc
		runtime.GC()
		runtime.ReadMemStats(&after)
		t.Logf("%-34s %4d KiB: alloc %4d MB, live %4d MB, err %v", c.name, len(c.src)>>10, total>>20, (int64(after.HeapAlloc)-int64(before.HeapAlloc))>>20, err)
		runtime.KeepAlive(tpl)
	}
}
```

Run: `go test -run TestZZParseMem -v . 2>&1 | grep zz_mem; rm zz_mem_test.go`
Before this release the figures were: 100k `{{a}}` 115 MB alloc / 25 MB live; 1 MiB `{{a b b …}}` 241 / 76; 1 MiB `{{a k=b …}}` 281 / 42. Use the figures you measure in Step 4, rounded to two significant digits.

- [x] **Step 3: README, "API at a glance".**
  - In the **Parsing** bullet, after "It also carries `Limits`.", add: "`NoBlocks` makes `Parse` reject block statements (`{{#…}}`, `{{^…}}`, raw blocks, partial blocks, decorator blocks), leaving the logic-less subset."
  - After the **Partials** bullet, add a new bullet: "**Inspection.** `Variables()` returns the paths into the root context that the template reads, e.g. `[[name] [user email]]`: at the top level and inside builtin `{{#if}}`/`{{#unless}}`, plus `@root.…` anywhere. Use it to check the data before rendering. Call it after registering helpers, since a bare name that matches a helper is not a variable. It does not look into partials: call it on each partial template."
  - In the **Errors** bullet, after the `*LimitError` sub-bullet, add: "`errors.Is` tells limits apart: a `*LimitError` matches `ErrTemplateSize`, `ErrParseDepth`, `ErrRenderDepth`, `ErrOutputSize`, `ErrOps` or `ErrExpressions`. A `NoBlocks` rejection is an `*Error` whose `Unwrap` returns `ErrBlocksNotAllowed`. Every other `*Error` unwraps to nil."
  - In the **Usage** paragraph, replace "`Example_untrusted` in the same file shows tightened limits, with the render under a deadline." with "`Example_untrusted` in the same file shows a setup for untrusted templates: `NoBlocks`, tightened limits, the error sentinels, `Variables()` and a render under a deadline."

- [x] **Step 4: README, "Security model".**
  - Add a table row after `MaxParseDepth`: `| `MaxExpressions` | 100,000 | Parse memory and CPU from sources made of many expressions. Every mustache, block and `{{else x}}` link, subexpression, partial, partial block, raw block and decorator counts as one; arguments and hash pairs don't. The lexer stops at the first expression over the limit, so a source over it costs only the part before that expression. For untrusted templates, tens or hundreds is typical. |`
  - In "Further guarantees", after **Sticky limits**, add: "**Logic-less templates.** With `Options.NoBlocks`, `Parse` rejects every block statement with an `*Error` at the first block's opening tag that wraps `ErrBlocksNotAllowed`. `RegisterPartial` parses each partial with the same Options. A partial registered with `RegisterPartialTemplate` renders with its own parse-time Options, so parse such partials with `NoBlocks` as well."
  - Replace the **Parse memory** bullet with the following, using the figures from Step 2: "**Parse memory.** Parsing allocates in proportion to the source, so `MaxTemplateSize` bounds it. `MaxExpressions` bounds it further for sources made of many expressions, because lexing stops at the first expression over the limit. At the defaults, 100,000 `{{a}}` (488 KiB) allocate about <A> MB, of which a <L> MB AST stays live with the `Template`. Arguments and hash pairs are not expressions, so a single expression with many of them is bounded by `MaxTemplateSize` alone: a 1 MiB `{{a k=b k=b …}}` allocates about <H> MB. For untrusted templates, lower both limits. At `MaxTemplateSize: 64 << 10`, parsing allocates at most about <S> MB." Here `<A>`/`<L>` are the "100k {{a}}" figures, `<H>` is the larger of the two 1 MiB argument-dense allocs, and `<S>` is the "64 KiB {{a k=b ...}}" alloc. Write the numbers, not the placeholders.

- [x] **Step 5: Check.**

Run: `go test ./... && grep -n "MaxExpressions\|NoBlocks\|Variables()\|ErrBlocksNotAllowed\|ErrExpressions" README.md`
Expected: `ok`, and matches in API at a glance, the Security model table, Further guarantees and Parse memory. `grep -n '<A>\|<L>\|<H>\|<S>' README.md` prints nothing.

- [x] **Step 6: Commit.**

```bash
git add README.md example_test.go
git commit -m "docs: NoBlocks, MaxExpressions, sentinels and Variables"
```

---

### Task 20: Release v0.2.0 ✅

**Files:** none (git, GitHub)

- [x] **Step 1: Full gate on `release/v0.2.0`.**

```bash
test -z "$(gofmt -l .)" && go vet ./...
go test -race ./...
go test -run TestSpec -v . | grep 'spec total'     # spec total: 632/632 pass, 0 fail, 0 skip
(cd bench && go test ./...)                         # the comparison module builds against ../
git status --short                                  # only the untracked requirements doc
```

Expected: everything green. `git status` shows only `?? 2026-11-17-go-handlebars-missing-api-design.md`.

- [x] **Step 2: Release notes.** Write `/tmp/go-handlebars-v0.2.0.md`:

```markdown
v0.2.0: template inspection and policy API

New
- `Limits.MaxExpressions` bounds how many expressions `Parse` accepts: every mustache, block and `{{else x}}` link, subexpression, partial, partial block, raw block and decorator counts as one. Lexing stops at the first expression over the limit, so together with `MaxTemplateSize` it bounds parse memory for expression-dense sources. Arguments, hash pairs and path segments are not counted; `MaxTemplateSize` alone bounds those.
- `Options.NoBlocks` rejects block statements at parse time (`{{#x}}`, `{{^x}}`, raw blocks, partial blocks, decorator blocks such as `{{#*inline}}`). The error is an `*Error` at the first block's opening tag; `errors.Is(err, ErrBlocksNotAllowed)` reports it.
- `Template.Variables()` lists the root-context paths a template reads (top level, builtin `if`/`unless` bodies, `@root.…` anywhere), so data can be checked before rendering.
- `errors.Is` sentinels: `ErrTemplateSize`, `ErrParseDepth`, `ErrRenderDepth`, `ErrOutputSize`, `ErrOps` and `ErrExpressions` match a `*LimitError` through the new `LimitError.Is`. `ErrBlocksNotAllowed` matches through the new `Error.Unwrap`.

Breaking changes
- `MaxExpressions` defaults to 100,000. A template with more than 100,000 expressions that parsed in v0.1.0 now fails with a `*LimitError` (`What: "expressions"`). Set `Limits.MaxExpressions` higher to accept it. The limit is checked while tokenizing, so it is reported ahead of syntax errors and `NoBlocks` rejections, like `MaxTemplateSize`.
- `Limits` and `Options` have new fields (`MaxExpressions`, `NoBlocks`), so unkeyed composite literals of them, such as `handlebars.Limits{1 << 20, 100, 100, 8 << 20, 1_000_000}`, no longer compile.
- `Error` has a new unexported field, so unkeyed composite literals such as `handlebars.Error{1, 0, "msg"}` outside the package no longer compile.
- Keyed literals are unaffected.

Spec suite: 632/632 (handlebars-spec 748917a, handlebars.js v4.7.9).
```

- [x] **Step 3: Merge and tag.**

```bash
git checkout main
git merge --ff-only release/v0.2.0
git tag -a v0.2.0 -F /tmp/go-handlebars-v0.2.0.md
git show v0.2.0 --stat | head -30
```

- [x] **Step 4: Publish.**

```bash
git push origin main v0.2.0
gh release create v0.2.0 --verify-tag --title "v0.2.0" --notes-file /tmp/go-handlebars-v0.2.0.md
gh release view v0.2.0
```

- [x] **Step 5: Verify the module resolves.**

```bash
GOPROXY=https://proxy.golang.org GOFLAGS=-mod=mod go list -m github.com/nikolaymatrosov/go-handlebars@v0.2.0
```

Expected: `github.com/nikolaymatrosov/go-handlebars v0.2.0`. The proxy can lag by a few minutes, so retry once if it is not there yet. Then delete the merged branch: `git branch -d release/v0.2.0`.

## v0.2.0 Self-Review Notes

- Requirements coverage. R1 is Task 14 (lexer-counted, Decision 1), R2 Task 15, R3 Task 16, R4 Task 17. The fuzzer criteria are Task 18 and the Docs section Task 19. The tag and release notes are Task 20.
- Test tables. The R1 table has 12 rows and the R4 table 20; both appear verbatim in Tasks 14 and 17. The count−1 rows are covered as in Decision 2.
- Consistency. `exprOpen` (Task 14) and `blockStart` (Task 16) are distinct `tokSet`s built with `setOf` from `parser.go`. `isBuiltin` (Task 17) is used by both `known` and `helperRef`. The `ErrBlocksNotAllowed` text in Task 16 and the example output in Task 19 match Decision 9.
- Placeholders. Task 19 Step 4 is the only step whose numbers come from a measurement (Step 2). Its `<A>`, `<L>`, `<H>` and `<S>` markers are checked by grep in Step 5.
