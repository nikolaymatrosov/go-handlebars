package handlebars

import (
	"strings"
	"testing"
	"time"
)

func init() {
	specTokenize = func(src string) ([]specToken, error) {
		toks, err := lex(src, DefaultLimits)
		if err != nil {
			return nil, err
		}
		out := make([]specToken, 0, len(toks))
		for _, t := range toks {
			if t.kind == tEOF || t.kind == tINVALID { // the JS spec tokenizer stops at both
				break
			}
			out = append(out, specToken{t.kind.String(), t.text})
		}
		return out, nil
	}
}

func TestLexPositions(t *testing.T) {
	toks, err := lex("ab\n{{foo}}\r\n {{ bar }}", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	want := []token{
		{tCONTENT, "ab\n", 1, 0, 0}, {tOPEN, "{{", 2, 0, 3}, {tID, "foo", 2, 2, 5}, {tCLOSE, "}}", 2, 5, 8},
		{tCONTENT, "\r\n ", 2, 7, 10}, {tOPEN, "{{", 3, 1, 13}, {tID, "bar", 3, 4, 16}, {tCLOSE, "}}", 3, 8, 20}, {tEOF, "", 3, 10, 22},
	}
	if len(toks) != len(want) {
		t.Fatalf("got %v", toks)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("token %d: got %v want %v", i, toks[i], want[i])
		}
	}
}

func TestLexLimitAndError(t *testing.T) {
	if _, err := lex("abcd", Limits{MaxTemplateSize: 3}); err == nil {
		t.Error("want LimitError")
	} else if _, ok := err.(*LimitError); !ok {
		t.Errorf("got %T", err)
	}
	if _, err := lex("{{!-- x", Limits{}); err == nil {
		t.Error("want lexical error for unterminated comment")
	}
}

// Unterminated scans must stay linear: each case sits near MaxTemplateSize and
// would take minutes if a failed forward search were retried per offset.
func TestLexLinearOnUnterminated(t *testing.T) {
	n := DefaultLimits.MaxTemplateSize
	cases := map[string]string{
		"bracket":     "{{" + strings.Repeat("[", n-2),
		"bang":        strings.Repeat("{{!", n/3),
		"dquote":      "{{" + strings.Repeat(`"`, n-2),
		"squote":      "{{" + strings.Repeat(`'`, n-2),
		"escdquote":   "{{" + strings.Repeat(`\"`, (n-2)/2),
		"id run":      "{{" + strings.Repeat("a", n-3) + "(",
		"digit run":   "{{" + strings.Repeat("1", n-3) + "(",
		"fraction":    "{{1." + strings.Repeat("1", n-5) + "(",
		"inverse ws":  "{{^" + strings.Repeat(" ", n-3),
		"comment end": "{{!--" + strings.Repeat("-", n-5),
		"raw":         "{{{{a}}}}" + strings.Repeat("{{{{/", (n-9)/5),
	}
	for name, src := range cases {
		start := time.Now()
		lex(src, Limits{MaxExpressions: n}) // result irrelevant; only the runtime matters
		d := time.Since(start)
		t.Logf("%s: %v", name, d)
		if d > time.Second {
			t.Errorf("%s: %v, want linear", name, d)
		}
	}
}

// Jison appends \b to the "else" literal: {{elsex}} is a mustache.
func TestLexElseWordBoundary(t *testing.T) {
	for src, want := range map[string]tokenKind{"{{elsex}}": tOPEN, "{{else x}}": tOPEN_INVERSE_CHAIN, "{{else(x)}}": tOPEN_INVERSE_CHAIN} {
		if toks, err := lex(src, Limits{}); err != nil || toks[0].kind != want {
			t.Errorf("lex(%q)[0] = %v, %v; want %v", src, toks, err, want)
		}
	}
}
