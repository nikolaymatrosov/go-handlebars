package handlebars

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type specTest struct {
	Description    string                     `json:"description"`
	It             string                     `json:"it"`
	Template       string                     `json:"template"`
	Data           json.RawMessage            `json:"data"`
	Expected       *string                    `json:"expected"`
	Helpers        json.RawMessage            `json:"helpers"`
	Partials       map[string]json.RawMessage `json:"partials"` // string or code object
	CompileOptions map[string]any             `json:"compileOptions"`
	RuntimeOptions map[string]json.RawMessage `json:"runtimeOptions"`
	Exception      any                        `json:"exception"` // true, or a string/regex naming the error (not compared)
	Message        string                     `json:"message"`   // human description only, never compared
	Decorators     json.RawMessage            `json:"decorators"`
	Number         string                     `json:"number"`
}

// specGate is the minimum pass count per file. Each task raises the gates
// of the files it fixes; lowering a gate is forbidden. 0 = report only.
var specGate = map[string]int{
	"tokenizer.json":          78,
	"parser.json":             79,
	"basic.json":              90,
	"blocks.json":             39,
	"builtins.json":           42,
	"data.json":               22,
	"helpers.json":            87,
	"partials.json":           61,
	"regressions.json":        33,
	"strict.json":             22,
	"string-params.json":      8,
	"subexpressions.json":     18,
	"track-ids.json":          17,
	"whitespace-control.json": 36,
}

// Task 1 hooks: tokenizer.json runner. Nil means not implemented (tests FAIL).
type specToken struct{ Name, Text string }

var specTokenize func(src string) ([]specToken, error)

// Task 2 hook: parser.json runner (AST printed like printer.js).
var specPrintAST func(src string) (string, error)

// omap preserves JSON object key order; implements KeyedCollection.
type omap struct {
	keys []string
	m    map[string]any
}

func (o *omap) Keys() []string           { return o.keys }
func (o *omap) Get(k string) (any, bool) { v, ok := o.m[k]; return v, ok }
func (o *omap) Len() int                 { return len(o.keys) }

// decodeOrdered parses JSON so objects become *omap, arrays []any, numbers
// float64 (JS has only doubles).
func decodeOrdered(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string, float64, bool, nil
	}
	if d == '[' {
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.Token() // ]
		return arr, err
	}
	o := &omap{m: map[string]any{}}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k := kt.(string)
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		if _, dup := o.m[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.m[k] = v
	}
	_, err = dec.Token() // }
	return o, err
}

func TestDecodeOrdered(t *testing.T) {
	v, err := decodeOrdered([]byte(`{"b":1,"a":{"d":2,"c":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*omap)
	a, _ := o.Get("a")
	if got := fmt.Sprint(o.Keys(), a.(*omap).Keys()); got != "[b a] [d c]" {
		t.Errorf("key order = %s", got)
	}
	if b, _ := o.Get("b"); b != float64(1) {
		t.Errorf("b = %#v, want float64(1)", b)
	}
}

// missingCode is a JS source with no (non-nil) specFuncs entry.
type missingCode struct{ hash, js string }

// errSkip reports every missing specFuncs entry hit while resolving a test.
type errSkip struct{ missing []missingCode }

func (e *errSkip) Error() string {
	var hs []string
	for _, m := range e.missing {
		hs = append(hs, m.hash)
	}
	return "missing specFuncs: " + strings.Join(hs, ", ")
}

// codeHash is the specFuncs key for a JS source.
func codeHash(js string) string {
	sum := sha1.Sum([]byte(js))
	return hex.EncodeToString(sum[:])[:12]
}

// absent marks a code object without JS (PHP-only); parents drop it.
type absentCode struct{}

// resolveCode replaces each {"!code":true,"javascript":...} object in v by
// specFuncs[codeHash(js)]. A code object without JS is dropped. Missing or nil
// entries yield *errSkip listing all of them.
func resolveCode(v any) (any, error) {
	var skip errSkip
	out := resolveWalk(v, &skip)
	if len(skip.missing) > 0 {
		return nil, &skip
	}
	if out == (absentCode{}) {
		out = nil
	}
	return out, nil
}

func resolveWalk(v any, skip *errSkip) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if r := resolveWalk(e, skip); r != (absentCode{}) {
				out = append(out, r)
			}
		}
		return out
	case *omap:
		if c, ok := x.m["!code"]; ok && c == true {
			js, ok := x.m["javascript"].(string)
			if !ok {
				return absentCode{}
			}
			h := codeHash(js)
			if f := specFuncs[h]; f != nil {
				return f
			}
			skip.missing = append(skip.missing, missingCode{h, js})
			return nil
		}
		if x.m["!sparsearray"] == true {
			n, _ := x.m["!length"].(float64)
			out := make([]any, int(n))
			for i := range out {
				out[i] = hole{}
				if e, ok := x.m[strconv.Itoa(i)]; ok {
					out[i] = resolveWalk(e, skip)
				}
			}
			return out
		}
		out := &omap{m: map[string]any{}}
		for _, k := range x.keys {
			if r := resolveWalk(x.m[k], skip); r != (absentCode{}) {
				out.keys = append(out.keys, k)
				out.m[k] = r
			}
		}
		return out
	}
	return v
}

var stubsPrinted = map[string]bool{}

// NOTE: stdout of passing packages is hidden by `go test` without -v, so
// HBSPEC_STUBS=1 needs -v to be visible.
// printStubs emits paste-ready specFuncs entries (HBSPEC_STUBS=1), each
// followed by the full JS as comments, once per hash per run.
func printStubs(err error) {
	var s *errSkip
	if os.Getenv("HBSPEC_STUBS") != "1" || !errors.As(err, &s) {
		return
	}
	for _, m := range s.missing {
		if stubsPrinted[m.hash] {
			continue
		}
		stubsPrinted[m.hash] = true
		first, _, _ := strings.Cut(m.js, "\n")
		if len(first) > 80 {
			first = first[:80]
		}
		fmt.Printf("\t%q: nil, // %s\n", m.hash, first)
		for _, l := range strings.Split(m.js, "\n") {
			fmt.Printf("\t//\t%s\n", l)
		}
	}
}

// buildOptions is the single compileOptions -> Options mapping.
// Unknown keys and wrong value types are errors (a FAIL) so nothing is silently ignored.
func buildOptions(co map[string]any) (Options, error) {
	var o Options
	for k, v := range co {
		b, isBool := v.(bool)
		if k != "knownHelpers" && !isBool {
			return o, fmt.Errorf("compileOption %q: want bool, got %T", k, v)
		}
		switch k {
		case "strict":
			o.Strict = b
		case "assumeObjects":
			o.AssumeObjects = b
		case "compat":
			o.Compat = b
		case "data":
			o.Data = &b
		case "knownHelpers":
			o.KnownHelpers = map[string]bool{}
			kh, ok := v.(map[string]any)
			if !ok {
				return o, fmt.Errorf("compileOption knownHelpers: want object, got %T", v)
			}
			for name, on := range kh {
				if o.KnownHelpers[name], ok = on.(bool); !ok {
					return o, fmt.Errorf("compileOption knownHelpers.%s: want bool, got %T", name, on)
				}
			}
		case "knownHelpersOnly":
			o.KnownHelpersOnly = b
		case "ignoreStandalone":
			o.IgnoreStandalone = b
		case "preventIndent":
			o.PreventIndent = b
		case "explicitPartialContext":
			o.ExplicitPartialContext = b
		case "noEscape":
			o.NoEscape = b
		case "stringParams":
			o.StringParams = b
		case "trackIds":
			o.TrackIDs = b
		default:
			return o, fmt.Errorf("unknown compileOption %q", k)
		}
	}
	return o, nil
}

type specStatus int

const (
	specPass specStatus = iota
	specFail
	specSkip
)

// decodeResolved decodes raw (may be empty) and resolves code objects.
func decodeResolved(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	v, err := decodeOrdered(raw)
	if err != nil {
		return nil, err
	}
	return resolveCode(v)
}

func runRender(tc specTest) (specStatus, string) {
	jsGlobalValue = nil // JS globals set by ported helpers start undefined per test
	data, err := decodeResolved(tc.Data)
	if err != nil {
		return failOrSkip(err)
	}
	var helpers, decorators any
	if helpers, err = decodeResolved(tc.Helpers); err != nil {
		return failOrSkip(err)
	}
	if decorators, err = decodeResolved(tc.Decorators); err != nil {
		return failOrSkip(err)
	}
	// JS registers some partials globally (Handlebars.registerPartial) and
	// passes others per render (options.partials); a Template has one
	// registry, so both go there.
	partials := map[string]string{}
	partialFns := map[string]Helper{}
	for name, raw := range tc.Partials {
		p, err := decodeResolved(raw)
		if err != nil {
			return failOrSkip(err)
		}
		switch p := p.(type) {
		case string:
			partials[name] = p
		case func(*HelperOptions) (any, error):
			partialFns[name] = p
		default:
			return specFail, fmt.Sprintf("partial %q: unsupported %T", name, p)
		}
	}

	opts, err := buildOptions(tc.CompileOptions)
	if err != nil {
		return specFail, err.Error()
	}
	tpl, err := ParseWithOptions(tc.Template, opts)
	if err != nil {
		return expectErr(tc, err)
	}
	if hm, ok := helpers.(*omap); ok {
		for _, name := range hm.keys {
			switch h := hm.m[name].(type) {
			case Helper:
				tpl.RegisterHelper(name, h)
			case func(*HelperOptions) (any, error):
				tpl.RegisterHelper(name, h)
			} // anything else (e.g. a plain string) is not a helper
		}
	}
	if dm, ok := decorators.(*omap); ok {
		for _, name := range dm.keys {
			if d, ok := dm.m[name].(func(map[string]any, *HelperOptions) error); ok {
				tpl.registerDecorator(name, d)
			}
		}
	}
	for name, src := range partials {
		if err := tpl.RegisterPartial(name, src); err != nil {
			return expectErr(tc, err)
		}
	}
	for name, h := range partialFns { // JS partial(context, options), ported as a Helper with this = context
		tpl.RegisterPartialFunc(name, func(ctx any) (string, error) {
			v, err := h(&HelperOptions{this: ctx})
			return jsStr(v), err
		})
	}

	var dataVars map[string]any
	if raw, ok := tc.RuntimeOptions["data"]; ok {
		// runtimeOptions.data is a JSON object (key order is irrelevant) that may hold code objects.
		d, err := decodeResolved(raw)
		if err != nil {
			return failOrSkip(err)
		}
		if om, ok := d.(*omap); ok {
			dataVars = om.m
		}
	}
	var sb strings.Builder
	err = tpl.RenderWithData(&sb, data, dataVars)
	if err != nil {
		return expectErr(tc, err)
	}
	if tc.expectsError() {
		return specFail, "expected an error, got output " + fmt.Sprintf("%q", sb.String())
	}
	if tc.Expected == nil {
		return specFail, "test has no expected output"
	}
	if sb.String() != *tc.Expected {
		return specFail, fmt.Sprintf("got %q, want %q", sb.String(), *tc.Expected)
	}
	return specPass, ""
}

func failOrSkip(err error) (specStatus, string) {
	printStubs(err)
	if errors.As(err, new(*errSkip)) {
		return specSkip, err.Error()
	}
	return specFail, err.Error()
}

// expectErr judges an error from parsing/rendering against tc.Exception.
func expectErr(tc specTest, err error) (specStatus, string) {
	if !tc.expectsError() {
		return specFail, err.Error()
	}
	return matchException(tc.Exception, err)
}

// matchException mirrors handlebars.js shouldThrow: true matches any error,
// "/re/" is a regexp over the message, any other string must equal it.
func matchException(want any, err error) (specStatus, string) {
	s, ok := want.(string)
	if !ok {
		return specPass, ""
	}
	if len(s) >= 2 && s[0] == '/' && s[len(s)-1] == '/' {
		if m, rerr := regexp.MatchString(s[1:len(s)-1], err.Error()); rerr != nil || !m {
			return specFail, fmt.Sprintf("error %q does not match %s (%v)", err.Error(), s, rerr)
		}
	} else if err.Error() != s {
		return specFail, fmt.Sprintf("error %q, want %q", err.Error(), s)
	}
	return specPass, ""
}

// specDir holds the corpus, a git submodule. Module zips omit
// submodules, so without it the spec tests skip rather than fail `go test all`.
var specDir = "handlebars-spec/spec"

// specPaths lists the corpus files, skipping t when the submodule is absent.
func specPaths(t *testing.T) []string {
	paths, _ := filepath.Glob(filepath.Join(specDir, "*.json"))
	if len(paths) == 0 {
		t.Skip("handlebars-spec submodule not checked out: git submodule update --init")
	}
	return paths
}

func specFiles(t *testing.T, kind func(name string) bool) []string {
	var out []string
	for _, p := range specPaths(t) {
		name := filepath.Base(p)
		if kind(name) && (os.Getenv("HBSPEC_FILE") == "" || os.Getenv("HBSPEC_FILE") == name) {
			out = append(out, p)
		}
	}
	return out
}

// runSpecFile runs one subtest per case and logs/gates the per-file summary.
func runSpecFile(t *testing.T, name string, n int, label func(i int) string, run func(i int) (specStatus, string)) {
	var count [3]int
	t.Run(name, func(t *testing.T) {
		for i := 0; i < n; i++ {
			lbl := label(i)
			if it := os.Getenv("HBSPEC_IT"); it != "" && !strings.Contains(lbl, it) {
				continue
			}
			t.Run(fmt.Sprintf("%d_%s", i, lbl), func(t *testing.T) {
				st, msg := run(i)
				count[st]++
				switch st {
				case specSkip:
					t.Skip(msg)
				case specFail:
					t.Log("FAIL:", msg) // gated per file, not per test
				}
			})
		}
	})
	t.Logf("%s: %d/%d pass, %d fail, %d skip", name, count[specPass], count[specPass]+count[specFail]+count[specSkip], count[specFail], count[specSkip])
	if os.Getenv("HBSPEC_FILE") != "" || os.Getenv("HBSPEC_IT") != "" {
		return
	}
	specTally[name] = count
	if g := specGate[name]; count[specPass] < g {
		t.Errorf("%s: %d pass is below gate %d", name, count[specPass], g)
	}
}

// specTally holds each file's [pass, fail, skip] from unfiltered runs, for
// TestSpecTotal.
var specTally = map[string][3]int{}

// specTotal is the corpus size: TestSpecTotal requires all of it to pass.
const specTotal = 632

func readSpec(t *testing.T, path string, into any) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestSpecTokenizer(t *testing.T) {
	for _, p := range specFiles(t, func(n string) bool { return n == "tokenizer.json" }) {
		var tests []struct {
			It       string      `json:"it"`
			Template string      `json:"template"`
			Expected []specToken `json:"expected"`
		}
		readSpec(t, p, &tests)
		runSpecFile(t, filepath.Base(p), len(tests), func(i int) string { return tests[i].It }, func(i int) (specStatus, string) {
			if specTokenize == nil {
				return specFail, "not implemented"
			}
			got, err := specTokenize(tests[i].Template)
			if err != nil {
				return specFail, err.Error()
			}
			if !reflect.DeepEqual(got, tests[i].Expected) {
				return specFail, fmt.Sprintf("got %v, want %v", got, tests[i].Expected)
			}
			return specPass, ""
		})
	}
}

func TestSpecParser(t *testing.T) {
	for _, p := range specFiles(t, func(n string) bool { return n == "parser.json" }) {
		var tests []specTest
		readSpec(t, p, &tests)
		runSpecFile(t, filepath.Base(p), len(tests), func(i int) string { return tests[i].It }, func(i int) (specStatus, string) {
			if specPrintAST == nil {
				return specFail, "not implemented"
			}
			got, err := specPrintAST(tests[i].Template)
			if tests[i].expectsError() {
				if err == nil {
					return specFail, "expected a parse error"
				}
				return matchException(tests[i].Exception, err)
			}
			if err != nil {
				return specFail, err.Error()
			}
			if tests[i].Expected == nil || got != *tests[i].Expected {
				return specFail, fmt.Sprintf("got %q, want %v", got, tests[i].Expected)
			}
			return specPass, ""
		})
	}
}

func TestSpecRender(t *testing.T) {
	for _, p := range specFiles(t, func(n string) bool { return n != "tokenizer.json" && n != "parser.json" }) {
		var tests []specTest
		readSpec(t, p, &tests)
		runSpecFile(t, filepath.Base(p), len(tests),
			func(i int) string { return tests[i].Description + "/" + tests[i].It },
			func(i int) (specStatus, string) { return runRender(tests[i]) })
	}
}

// expectsError: exception is true, or a string/regex of the expected error.
func (tc specTest) expectsError() bool {
	return tc.Exception != nil && tc.Exception != false
}

// TestSpecTotal runs after the spec runners (it is declared after them)
// and requires the whole corpus to pass, so a regression fails even if a
// gate was miscounted. It skips when a filter (or -shuffle) left a file
// unrun.
func TestSpecTotal(t *testing.T) {
	if len(specTally) != len(specGate) {
		t.Skipf("ran %d of %d spec files", len(specTally), len(specGate))
	}
	var sum [3]int
	for _, c := range specTally {
		for i, n := range c {
			sum[i] += n
		}
	}
	t.Logf("spec total: %d/%d pass, %d fail, %d skip", sum[specPass], specTotal, sum[specFail], sum[specSkip])
	if sum[specPass] != specTotal || sum[specFail] != 0 || sum[specSkip] != 0 {
		t.Errorf("spec: %d pass, %d fail, %d skip; want %d pass, 0 fail, 0 skip", sum[specPass], sum[specFail], sum[specSkip], specTotal)
	}
}

// Without the submodule every corpus test skips instead of failing.
func TestSpecSkipsWithoutSubmodule(t *testing.T) {
	defer func(dir string) { specDir = dir }(specDir)
	specDir = t.TempDir()
	for name, test := range map[string]func(*testing.T){
		"tokenizer": TestSpecTokenizer, "parser": TestSpecParser, "render": TestSpecRender,
		"no panic": TestParseSpecTemplatesNoPanic,
	} {
		t.Run(name, func(t *testing.T) {
			test(t)
			t.Error("did not skip") // a skip never returns here
		})
	}
}
