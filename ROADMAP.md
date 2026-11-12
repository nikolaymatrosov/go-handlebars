# go-handlebars Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go Handlebars library that passes all 632 tests of `jbboehr/handlebars-spec` (the JSON export of the handlebars.js test suite) and is safe to run on untrusted templates: every parse and render is bounded by explicit limits (bytes, depth, operation count), and rendering is allocation-frugal (buffers, never string concatenation in hot paths).

**Architecture:** Three classic stages — hand-written lexer → recursive-descent parser producing an immutable AST → tree-walking renderer writing into a counting output buffer. A compiled `*Template` is immutable and safe for concurrent rendering. A `Limits` struct is threaded through both parse and render; every byte written, every nesting level, and every substitution/iteration is accounted against it.

**Tech Stack:** Go ≥ 1.22, standard library only (no third-party deps). Test corpus: `handlebars-spec/spec/*.json` (git submodule, already added). Reference implementation vendored at `handlebars-spec/handlebars.js/` (nested submodule, already initialized).

## Global Constraints

- **Zero third-party dependencies.** `go.mod` lists nothing but the module itself.
- Module path: `github.com/nikthespirit/go-handlebars`, single package `handlebars` at repo root. Internal test tooling may live in subdirs.
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

- [x] **Step 1: `go mod init github.com/nikthespirit/go-handlebars`** (adjust path if the repo gets a different remote later).

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

### Task 3: Core renderer → basic.json 90/90 ✅ (86/90; 4 need with/unless → Task 6)

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

### Task 4: Blocks → blocks.json 31/39 (all except 8 decorator tests) ✅ (23/39; 8 need builtins/@data → T6/T7, 8 decorators → T10)

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

### Task 5: Helpers API → helpers.json green except stringParams/trackIds/knownHelpers edge set ✅ (75/87; 12 need if/unless/with → Task 6)

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

### Task 6: Builtins + data frames → builtins.json 42/42

**Files:**

- Create: `builtins.go`
- Modify: `render.go` (dataFrame plumbing), `spec_funcs_test.go`

**Interfaces:**

- Consumes: `HelperOptions.FnTo(w, ctx, blockParams, dataVars)`.
- Produces: default registry (every new Template gets it; user registrations shadow): `each` (arrays, KeyedCollection in Keys() order, plain maps in sorted-key order, sets `@index @key @first @last`, block params `|value key|`), `if`/`unless` (`includeZero` hash), `with` (context push + block param + else), `lookup` (safe, uses `lookupProperty`), `log` (writes to a `Logger func(level string, args ...any)` field on Template, default no-op; `level` hash arg). Port each from `lib/handlebars/helpers/<name>.js`.
- `dataFrame`: created by `each` iterations (`createFrame` semantics: copy-on-write child of parent frame; `@../index` reaches parent frame via `pathExpr.depth` on data paths).

- [ ] **Step 1: Implement dataFrame + `@` path evaluation** in `render.go` (pathExpr with `data=true`: resolve from current frame, `depth` walks parent frames).

- [ ] **Step 2: Implement the six builtins** with `FnTo` (no intermediate strings), `s.step()` per iteration.

- [ ] **Step 3: Port remaining specFuncs for builtins.json; run to 42/42.** Prior files stay green.

- [ ] **Step 4: Commit** `feat: builtin helpers + data frames (builtins 42/42)`.

---

### Task 7: @data semantics → data.json 22/22

**Files:**

- Modify: `render.go`, `handlebars.go` (`RenderWithData` initial frame, `Options.Data=false` disables frame creation), `spec_funcs_test.go`

- [ ] **Step 1:** `runtimeOptions.data` (27 tests corpus-wide) seeds the root frame — harness already passes it via `RenderWithData`. Implement `@root` (auto-set to root context), nested overrides, `Options.Data == false` → data tracking off (16 tests; `{{@foo}}` then resolves nil / helpers see no frame).

- [ ] **Step 2:** Run data.json to **22/22**; re-run full suite for regressions.

- [ ] **Step 3: Commit** `feat: @data variables (data 22/22)`.

---

### Task 8: Subexpressions → subexpressions.json 16/18 (2 stringParams skips)

**Files:**

- Modify: `render.go`/`helpers.go` (evaluate `subExpr` params: inner helper call whose return value becomes the arg; nested depth counts toward `MaxRenderDepth` via `s.depth`), `spec_funcs_test.go`

- [ ] **Step 1:** Implement `evalExpr` for `subExpr` (always a helper call — unknown name → `helperMissing`, matching JS behavior incl. its error `"<name> doesn't match <other>"`-style messages in corpus).
- [ ] **Step 2:** Run to **16 pass / 2 skip (stringParams) / 0 fail**. Commit `feat: subexpressions (16/18)`.

---

### Task 9: Partials → partials.json 61/61

**Files:**

- Create: `partials.go`
- Modify: `render.go`, `spec_funcs_test.go`

**Interfaces:**

- Produces: resolution order (inline partials from enclosing blocks → registered partials); dynamic partials `{{> (expr)}}`; custom context `{{> p ctx}}` and hash → context merge (hash-only → hash as context extension, `ExplicitPartialContext` forces hash/explicit only); partial blocks `{{#> p}}fallback{{/p}}` with `@partial-block`; inline partials `{{#*inline "name"}}` (store program in render-state scope chain — this is the one decorator the core needs; generic decorators are Task 10); failover error `The partial <name> could not be found`; indentation: each line of partial output indented by `partialNode.indent` unless `PreventIndent` — implement in `outWriter` as an active-indent mode (scan written chunks for `\n`, inject indent; no string rebuild).
- **Security:** every partial expansion does `s.depth++` + check — this is the recursive-partial bomb guard; add `s.step()` too.

- [ ] **Step 1:** Implement static/dynamic/context/hash resolution + missing-partial error.
- [ ] **Step 2:** Partial blocks + `@partial-block` (nested: each block pushes a frame var).
- [ ] **Step 3:** Inline partials incl. shadowing rules; `Compat` partial tests (4).
- [ ] **Step 4:** Indentation + `PreventIndent` (1 test) + standalone partial indent capture from Task 2.
- [ ] **Step 5:** Run partials.json to **61/61**; full-suite regression check. Commit `feat: partials (61/61)`.

---

### Task 10: Whitespace render tests, strict mode, regressions, decorators

**Files:**

- Modify: `render.go` (strict), `parser.go` (decorator AST already parsed in Task 2), `helpers.go` (decorator exec), `spec_funcs_test.go`

- [ ] **Step 1: whitespace-control.json → 36/36** (mostly already green from Task 2's AST strip flags; fix evaluator's handling of `strip` on content boundaries).
- [ ] **Step 2: strict.json → 22/22.** `Strict`: missing *property on existing-path lookups* and missing helpers → `*Error` with messages like `"<name>" not defined in <context>` (copy exact strings from the corpus `message` fields). `AssumeObjects`: skip existence checks on intermediate segments (nil deref must still return a clean error, never panic).
- [ ] **Step 3: decorators (blocks.json 8 tests) → blocks.json 39/39.** Minimal decorator machinery mirroring `runtime.js`: decorators run before their block's program, can replace/wrap the program's helper fn and mutate a `props` map. Internal `type decorator func(prog *boundProgram, props map[string]any, o *HelperOptions) (*boundProgram, error)`; harness registers test decorators from `specFuncs`. Built-in: `inline` (already used by Task 9 — refactor it onto this mechanism if that is a simplification, otherwise leave it; `ponytail:` note the choice).
- [ ] **Step 4: regressions.json → 33/33.** Grab-bag; expect fixes across value.go/render.go. Add `msgAliases` entries as needed.
- [ ] **Step 5: Commit** `feat: strict mode, decorators, regressions (whitespace 36, strict 22, blocks 39, regressions 33)`.

---

### Task 11: Legacy options → string-params.json 8/8, track-ids.json 17/17, remaining skips → 0

**Files:**

- Modify: `helpers.go`, `render.go`, `spec_funcs_test.go`

- [ ] **Step 1: `StringParams`** (10 tests): args passed to helpers as path *strings* instead of resolved values; `options.types`/`options.hashTypes` arrays exposed on `HelperOptions` as `Types []string` / `HashTypes map[string]string` (add these two exported fields — they are additive, allowed). Mirror `compiler.js` stringParams mode.
- [ ] **Step 2: `TrackIDs`** (17 tests): `options.ids`/`hashIds` → `IDs []string` / `HashIDs map[string]string` fields; ids are contextual source paths (see `track-ids.json` expectations and `javascript-compiler.js` `trackIds` handling).
- [ ] **Step 3: Full sweep:** `go test ./... | tail -20` → **632/632 pass, 0 fail, 0 skip**. Fix stragglers.
- [ ] **Step 4: Commit** `feat: stringParams + trackIds; full handlebars-spec green (632/632)`.

---

### Task 12: Adversarial security tests + fuzzing

**Files:**

- Create: `limits_test.go`, `fuzz_test.go`

- [ ] **Step 1: Write the limit tests** (the spec corpus does not cover limits — these are mandatory hand-written tests):

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

- [ ] **Step 2: Fuzzing.**

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

- [ ] **Step 3: Commit** `test: adversarial limit tests + fuzzing`.

---

### Task 13: Performance — benchmarks and allocation audit

**Files:**

- Create: `bench_test.go`; targeted edits across `render.go`/`value.go`/`escape.go`

- [ ] **Step 1: Benchmarks** with `b.ReportAllocs()`: (a) static-heavy template, (b) `{{#each}}` over 1000 items with 3 fields, (c) partial-heavy, (d) escape-heavy. Baseline numbers recorded in the commit message.
- [ ] **Step 2: Audit with `-benchmem` + profiles.** Required outcomes: zero allocations per render for (a) when using `RenderTo` with a reusable writer (pool `renderState` + `outWriter` in a `sync.Pool`); no per-iteration allocations in (b) beyond unavoidable `any` boxing of looked-up values; `jsNumString`/numbers formatted via `strconv.AppendFloat` into `outWriter.scratch`.
- [ ] **Step 3: `testing.AllocsPerRun` assertions** in `bench_test.go` for case (a) (`== 0`) and an escape-only write (`== 0`), so regressions fail CI, not just benchmarks.
- [ ] **Step 4: README.md**: API example, security model (Limits table with defaults and what attack each stops), spec-compliance statement (632/632, corpus commit hash), concurrency contract. Written for library users.
- [ ] **Step 5: Commit** `perf: pooled render state, zero-alloc static render + README`.

---

## Self-Review Notes (already applied)

- Handlebars cannot pass mustache/spec (delimiters, lambdas) — the `spec/` submodule stays as reference only; no task targets it. User confirmed handlebars-spec is the target.
- All 11 observed `compileOptions` map to `Options` fields; `exception` tests (54) and `message` matching policy live in the Task 0 harness; the 36 data-embedded functions are covered by `resolveCode` walking `data` (Task 0) + lambda-call semantics (Task 3).
- Gate arithmetic: 78+79 (Tasks 1–2) + 90 (T3) + 39 (T4+T10) + 87 (T5+T11) + 42 (T6) + 22 (T7) + 18 (T8+T11) + 61 (T9) + 36+22+33 (T10) + 8+17 (T11) = 632.
- Decorator tests and `inline` partials share machinery; Task 9 implements `inline` standalone, Task 10 may refactor — flagged, not mandated.
