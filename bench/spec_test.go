package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aymerick/raymond"
	handlebars "github.com/nikolaymatrosov/go-handlebars"
)

// TestSpecSubset runs the handlebars-spec render tests that need no JS
// code (no code object in helpers, decorators, partials, data or
// runtimeOptions; sparse arrays excluded too, they need the root harness's
// internal hole type) through both engines and logs per-file pass counts
// plus every raymond failure by category (go test -run TestSpecSubset -v).
// An exception test passes on any error, or for raymond a panic.
// Raymond's failures are data, not errors; a go-handlebars failure is.
func TestSpecSubset(t *testing.T) {
	paths, _ := filepath.Glob("../handlebars-spec/spec/*.json")
	if len(paths) == 0 {
		t.Skip("handlebars-spec submodule not checked out")
	}
	var total [3]int // tests, go-handlebars pass, raymond pass
	panics := 0
	failures := map[string][]string{} // raymond category -> lines
	for _, p := range paths {
		name := filepath.Base(p)
		if name == "tokenizer.json" || name == "parser.json" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var tests []specTest
		if err := json.Unmarshal(raw, &tests); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var n, ghPass, rayPass, filePanics int
		for _, tc := range tests {
			if tc.needsCode() {
				continue
			}
			n++
			label := name + " / " + tc.Description + " / " + tc.It
			if gh := runGH(tc); tc.judge(gh) != "" {
				t.Errorf("go-handlebars FAIL %s: %s: %s", label, tc.judge(gh), tc.detail(gh))
			} else {
				ghPass++
			}
			res := runRaymond(tc)
			if res.panicked {
				filePanics++
			}
			cat := res.category
			if cat == "" {
				cat = tc.judge(res.outcome)
			}
			if cat == "" {
				rayPass++
				continue
			}
			detail := tc.detail(res.outcome)
			if cat == unsupported {
				detail = fmt.Sprint(tc.CompileOptions) // sorted keys
			} else if res.alt != "" {
				detail = fmt.Sprintf("%q, then %q", res.out, res.alt)
			}
			cat = tc.feature(cat)
			failures[cat] = append(failures[cat], label+": "+clip(detail))
		}
		t.Logf("%s: %d tests, go-handlebars %d/%d, raymond %d/%d (%d panics)", name, n, ghPass, n, rayPass, n, filePanics)
		total[0] += n
		total[1] += ghPass
		total[2] += rayPass
		panics += filePanics
	}
	t.Logf("total: %d tests, go-handlebars %d/%d, raymond %d/%d (%d panics)", total[0], total[1], total[0], total[2], total[0], panics)
	cats := make([]string, 0, len(failures))
	for c := range failures {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool {
		if len(failures[cats[i]]) != len(failures[cats[j]]) {
			return len(failures[cats[i]]) > len(failures[cats[j]])
		}
		return cats[i] < cats[j]
	})
	for _, c := range cats {
		t.Logf("raymond failures, %s: %d", c, len(failures[c]))
		for _, l := range failures[c] {
			t.Logf("    %s", l)
		}
	}
}

type specTest struct {
	Description    string                     `json:"description"`
	It             string                     `json:"it"`
	Template       string                     `json:"template"`
	Data           json.RawMessage            `json:"data"`
	Expected       *string                    `json:"expected"`
	Helpers        json.RawMessage            `json:"helpers"`
	Decorators     json.RawMessage            `json:"decorators"`
	Partials       map[string]json.RawMessage `json:"partials"`
	CompileOptions map[string]any             `json:"compileOptions"`
	RuntimeOptions map[string]json.RawMessage `json:"runtimeOptions"`
	Exception      any                        `json:"exception"`
}

// needsCode reports a JS code object (or a sparse array) anywhere in the
// test's inputs. Helpers without code (one test has a string) are ignored
// by both engines, as in the root harness.
func (tc specTest) needsCode() bool {
	raws := []json.RawMessage{tc.Helpers, tc.Decorators, tc.Data}
	for _, r := range tc.Partials {
		raws = append(raws, r)
	}
	for _, r := range tc.RuntimeOptions {
		raws = append(raws, r)
	}
	for _, r := range raws {
		if bytes.Contains(r, []byte(`"!code"`)) || bytes.Contains(r, []byte(`"!sparsearray"`)) {
			return true
		}
	}
	return false
}

func (tc specTest) expectsError() bool { return tc.Exception != nil && tc.Exception != false }

// outcome is one engine's result for a test.
type outcome struct {
	out      string
	err      error
	panicked bool
}

// judge returns "" for a pass, else the failure category.
func (tc specTest) judge(o outcome) string {
	failed := o.err != nil || o.panicked
	switch {
	case tc.expectsError():
		if failed {
			return ""
		}
		return "no error (exception expected)"
	case o.panicked:
		return "panic"
	case o.err != nil:
		if strings.HasPrefix(o.err.Error(), "Parse error") {
			return "parse error"
		}
		return "render error"
	case tc.Expected == nil:
		return "test has no expected output"
	case o.out != *tc.Expected:
		return "wrong output"
	}
	return ""
}

func (tc specTest) detail(o outcome) string {
	if o.err != nil {
		return o.err.Error()
	}
	if tc.Expected != nil {
		return fmt.Sprintf("got %q, want %q", o.out, *tc.Expected)
	}
	return fmt.Sprintf("got %q", o.out)
}

// feature refines a raymond failure category by the handlebars.js 4
// feature the template uses that raymond (handlebars.js 3 grammar) lacks.
func (tc specTest) feature(cat string) string {
	src := tc.Template
	for _, p := range tc.Partials {
		src += string(p)
	}
	switch {
	case cat == unsupported:
		return cat
	case strings.Contains(src, "#*inline"):
		return "inline partials ({{#*inline}})"
	case strings.Contains(src, "#>") || strings.Contains(src, "@partial-block"):
		return "partial blocks ({{#> p}}, @partial-block)"
	}
	return cat
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// runGH: objects decode to an insertion-ordered KeyedCollection, as the
// library documents for JS key order.
func runGH(tc specTest) outcome {
	var opts handlebars.Options
	for k, v := range tc.CompileOptions {
		b, _ := v.(bool)
		switch k {
		case "strict":
			opts.Strict = b
		case "assumeObjects":
			opts.AssumeObjects = b
		case "compat":
			opts.Compat = b
		case "data":
			opts.Data = &b
		case "knownHelpersOnly":
			opts.KnownHelpersOnly = b
		case "ignoreStandalone":
			opts.IgnoreStandalone = b
		case "preventIndent":
			opts.PreventIndent = b
		case "explicitPartialContext":
			opts.ExplicitPartialContext = b
		case "noEscape":
			opts.NoEscape = b
		default:
			return outcome{err: fmt.Errorf("adapter: compileOption %q not mapped", k)}
		}
	}
	data, err := decodeOrdered(tc.Data)
	if err != nil {
		return outcome{err: err}
	}
	tpl, err := handlebars.ParseWithOptions(tc.Template, opts)
	if err != nil {
		return outcome{err: err}
	}
	for name, raw := range tc.Partials {
		var src string
		if err := json.Unmarshal(raw, &src); err != nil {
			return outcome{err: err}
		}
		if err := tpl.RegisterPartial(name, src); err != nil {
			return outcome{err: err}
		}
	}
	var vars map[string]any
	if raw, ok := tc.RuntimeOptions["data"]; ok {
		d, err := decodeOrdered(raw)
		if err != nil {
			return outcome{err: err}
		}
		if om, ok := d.(*omap); ok { // as the root harness: only an object seeds @data
			vars = om.m
		}
	}
	var sb strings.Builder
	err = tpl.RenderWithData(&sb, data, vars)
	return outcome{out: sb.String(), err: err}
}

// rayResult is a raymond outcome plus a category when the test could not
// be run as written or its runs disagreed.
type rayResult struct {
	outcome
	category string
	alt      string // an output that differs from out
}

const unsupported = "unsupported compile option (raymond has none)"

// rayRuns: Go map iteration order is random, so each test is executed
// this many times; differing results count as a failure.
const rayRuns = 20

// runRaymond: data decodes the usual Go way (map[string]any, float64);
// raymond has no ordered-object type. Raymond has no compile options, so a
// test setting one to a non-default value is not run.
func runRaymond(tc specTest) rayResult {
	for k, v := range tc.CompileOptions {
		if def := k == "data"; v != def {
			if m, ok := v.(map[string]any); !(ok && len(m) == 0) {
				return rayResult{category: unsupported}
			}
		}
	}
	var data any
	if len(tc.Data) > 0 {
		if err := json.Unmarshal(tc.Data, &data); err != nil {
			return rayResult{outcome: outcome{err: err}}
		}
	}
	var tpl *raymond.Template
	if o := rayCall(func() (err error) { tpl, err = raymond.Parse(tc.Template); return }); o.err != nil {
		return rayResult{outcome: o}
	}
	for name, raw := range tc.Partials {
		var src string
		if err := json.Unmarshal(raw, &src); err != nil {
			return rayResult{outcome: outcome{err: err}}
		}
		tpl.RegisterPartial(name, src) // parsed lazily, at Exec
	}
	var frame *raymond.DataFrame
	if raw, ok := tc.RuntimeOptions["data"]; ok {
		var vars map[string]any
		_ = json.Unmarshal(raw, &vars) // only an object seeds @data
		frame = raymond.NewDataFrame()
		for k, v := range vars {
			frame.Set(k, v)
		}
	}
	var res rayResult
	for i := 0; i < rayRuns; i++ {
		var out string
		o := rayCall(func() (err error) { out, err = tpl.ExecWith(data, frame); return })
		o.out = out
		if i == 0 {
			res.outcome = o
		} else if o.out != res.out || fmt.Sprint(o.err) != fmt.Sprint(res.err) {
			res.category, res.alt = "nondeterministic output (Go map order)", o.out
			break
		}
	}
	return res
}

// rayCall runs f, turning a panic (raymond re-panics runtime errors and
// non-error values) into a panicked outcome.
func rayCall(f func() error) (o outcome) {
	defer func() {
		if p := recover(); p != nil {
			o = outcome{err: fmt.Errorf("panic: %v", p), panicked: true}
		}
	}()
	return outcome{err: f()}
}

// omap preserves JSON object key order; implements handlebars.KeyedCollection.
type omap struct {
	keys []string
	m    map[string]any
}

func (o *omap) Keys() []string           { return o.keys }
func (o *omap) Get(k string) (any, bool) { v, ok := o.m[k]; return v, ok }
func (o *omap) Len() int                 { return len(o.keys) }

// decodeOrdered parses JSON so objects become *omap, arrays []any, numbers
// float64; empty input is nil.
func decodeOrdered(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
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
		return tok, nil
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
		_, err := dec.Token()
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
	_, err = dec.Token()
	return o, err
}
