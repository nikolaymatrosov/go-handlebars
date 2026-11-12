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
		{tCONTENT, "ab\n", 1, 0}, {tOPEN, "{{", 2, 0}, {tID, "foo", 2, 2}, {tCLOSE, "}}", 2, 5},
		{tCONTENT, "\r\n ", 2, 7}, {tOPEN, "{{", 3, 1}, {tID, "bar", 3, 4}, {tCLOSE, "}}", 3, 8}, {tEOF, "", 3, 10},
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
		lex(src, Limits{}) // result irrelevant; only the runtime matters
		d := time.Since(start)
		t.Logf("%s: %v", name, d)
		if d > time.Second {
			t.Errorf("%s: %v, want linear", name, d)
		}
	}
}
