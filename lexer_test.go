package handlebars

import "testing"

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
