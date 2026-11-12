package handlebars

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Lexer states, mirroring the flex start conditions in handlebars.l.
// INITIAL is the empty stack.
type lexState uint8

const (
	stMu lexState = iota
	stEmu
	stRaw
	stCom
)

// isJSSpace reports whether r matches the JS regexp \s class.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// spaceAt returns the byte width of the whitespace char at s[i], or 0.
func spaceAt(s string, i int) int {
	if i >= len(s) {
		return 0
	}
	if c := s[i]; c < 0x80 {
		if isJSSpace(rune(c)) {
			return 1
		}
		return 0
	}
	r, w := utf8.DecodeRuneInString(s[i:])
	if isJSSpace(r) {
		return w
	}
	return 0
}

func skipSpace(s string, i int) int {
	for w := spaceAt(s, i); w > 0; w = spaceAt(s, i) {
		i += w
	}
	return i
}

// idRune reports whether r may appear in an ID (the negated class in handlebars.l).
func idRune(r rune) bool {
	switch {
	case isJSSpace(r):
		return false
	case r < 128:
		switch {
		case r >= '!' && r <= '#', r >= '%' && r <= ',', r == '.', r == '/',
			r >= ';' && r <= '>', r == '@', r >= '[' && r <= '^', r == '`', r >= '{' && r <= '~':
			return false
		}
	}
	return true
}

// lookahead reports whether s[i] is in set (false at end of input).
func lookahead(s string, i int, set string, space bool) bool {
	if i >= len(s) {
		return false
	}
	if strings.IndexByte(set, s[i]) >= 0 {
		return true
	}
	return space && spaceAt(s, i) > 0
}

const (
	laID  = "=~}/.)|" // LOOKAHEAD (plus \s)
	laLit = "~})"     // LITERAL_LOOKAHEAD (plus \s)
)

// scanQuoted scans a quote-delimited literal starting at s[i] (the opening
// quote), where esc+q is an escaped closing quote. Returns the index just past
// the closing quote, or -1. Matches the regexp q(\\q|[^q])*q including its
// backtracking: when no closing quote exists, the last escaped pair closes it.
func scanQuoted(s string, i int, q byte) int {
	lastPair := -1
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\' && j+1 < len(s) && s[j+1] == q:
			lastPair = j + 1
			j++
		case s[j] == q:
			return j + 1
		}
	}
	if lastPair >= 0 {
		return lastPair + 1
	}
	return -1
}

// lex scans src into tokens, ending with tEOF.
func lex(src string, limits Limits) ([]token, error) {
	limits = limits.withDefaults()
	if len(src) > limits.MaxTemplateSize {
		return nil, &LimitError{What: "template size", Max: limits.MaxTemplateSize}
	}

	var (
		toks  []token
		stack []lexState
		pos   int
		// incremental line/col tracking; cur is the offset line/col describe
		cur, line, col = 0, 1, 0
	)
	locate := func(off int) {
		for ; cur < off; cur++ {
			switch b := src[cur]; {
			case b == '\n', b == '\r' && (cur+1 >= len(src) || src[cur+1] != '\n'):
				line, col = line+1, 0
			case b == '\r':
			case b < 0x80, b >= 0xc0 && b < 0xf0:
				col++
			case b >= 0xf0:
				col += 2
			}
		}
	}
	emit := func(k tokenKind, start int, text string) {
		locate(start)
		toks = append(toks, token{k, text, line, col})
	}
	fail := func(off int) ([]token, error) {
		locate(off)
		return nil, &Error{Line: line, Column: col, Message: "Lexical error on line " + itoa(line) + ". Unrecognized text."}
	}
	pop := func() { stack = stack[:len(stack)-1] }
	has := func(i int, p string) bool { return strings.HasPrefix(src[i:], p) }

	for {
		if pos >= len(src) {
			emit(tEOF, pos, "")
			return toks, nil
		}
		if len(stack) == 0 { // INITIAL
			idx := strings.Index(src[pos:], "{{")
			if idx < 0 {
				emit(tCONTENT, pos, src[pos:])
				pos = len(src)
				continue
			}
			end := pos + idx
			text := src[pos:end]
			switch {
			case strings.HasSuffix(text, `\\`):
				text = text[:len(text)-1]
				stack = append(stack, stMu)
			case strings.HasSuffix(text, `\`):
				text = text[:len(text)-1]
				stack = append(stack, stEmu)
			default:
				stack = append(stack, stMu)
			}
			if text != "" {
				emit(tCONTENT, pos, text)
			}
			pos = end
			continue
		}
		switch stack[len(stack)-1] {
		case stEmu:
			if len(src)-pos < 2 {
				return fail(pos)
			}
			q := pos + 2
			for ; q < len(src); q++ {
				if has(q, "{{") || has(q, `\{{`) || has(q, `\\{{`) {
					break
				}
			}
			pop()
			emit(tCONTENT, pos, src[pos:q])
			pos = q
		case stRaw:
			switch {
			case has(pos, "{{{{") && pos+4 < len(src) && src[pos+4] != '/':
				stack = append(stack, stRaw)
				emit(tCONTENT, pos, src[pos:pos+4])
				pos += 4
				continue
			case has(pos, "{{{{/"):
				// "{{{{/" name, then "}}}}" (the grammar's lookahead char is the first "}")
				j := pos + 5
				for j < len(src) {
					r, w := utf8.DecodeRuneInString(src[j:])
					if !idRune(r) {
						break
					}
					j += w
				}
				if j > pos+5 && has(j, "}}}}") {
					pop()
					if len(stack) > 0 && stack[len(stack)-1] == stRaw {
						emit(tCONTENT, pos, src[pos:j+4])
					} else {
						emit(tEND_RAW_BLOCK, pos, src[pos+5:j])
					}
					pos = j + 4
					continue
				}
			}
			idx := strings.Index(src[pos+1:], "{{{{")
			if idx < 0 {
				return fail(pos)
			}
			emit(tCONTENT, pos, src[pos:pos+1+idx])
			pos += 1 + idx
		case stCom:
			idx := strings.Index(src[pos:], "--")
			for idx >= 0 {
				e := pos + idx + 2
				if e < len(src) && src[e] == '~' {
					e++
				}
				if has(e, "}}") {
					pop()
					emit(tCOMMENT, pos, src[pos:e+2])
					pos = e + 2
					break
				}
				n := strings.Index(src[pos+idx+1:], "--")
				if n < 0 {
					idx = -1
				} else {
					idx += 1 + n
				}
			}
			if idx < 0 {
				return fail(pos)
			}
		case stMu:
			k, text, end := lexMu(src, pos)
			switch k {
			case muSkip:
			case muComment: // "{{!--": hand off to the comment state without consuming
				stack[len(stack)-1] = stCom
				continue
			default:
				switch k {
				case tINVERSE, tCLOSE, tCLOSE_UNESCAPED:
					pop()
				case tCLOSE_RAW_BLOCK:
					stack[len(stack)-1] = stRaw
				case tCOMMENT:
					pop()
				}
				emit(k, pos, text)
			}
			pos = end
		}
	}
}

const (
	muSkip    tokenKind = 254
	muComment tokenKind = 255
)

// lexMu matches one token in the mu state at src[pos:] (first matching rule
// wins, in handlebars.l order). It returns the kind, token text and end offset.
func lexMu(src string, pos int) (k tokenKind, text string, end int) {
	tok := func(k tokenKind, end int) (tokenKind, string, int) { return k, src[pos:end], end }
	has := func(i int, p string) bool { return strings.HasPrefix(src[i:], p) }
	c := src[pos]
	switch c {
	case '(':
		return tok(tOPEN_SEXPR, pos+1)
	case ')':
		return tok(tCLOSE_SEXPR, pos+1)
	case '{':
		if has(pos, "{{{{") {
			return tok(tOPEN_RAW_BLOCK, pos+4)
		}
		if !has(pos, "{{") {
			break
		}
		p := pos + 2
		if has(p, "~") {
			p++
		}
		// "else" and "^" forms may be followed by \s*~?}} (INVERSE)
		closeAt := func(i int) int { // end of \s*~?}} at i, or -1
			i = skipSpace(src, i)
			if has(i, "~") {
				i++
			}
			if has(i, "}}") {
				return i + 2
			}
			return -1
		}
		switch {
		case has(p, "#>"):
			return tok(tOPEN_PARTIAL_BLOCK, p+2)
		case has(p, ">"):
			return tok(tOPEN_PARTIAL, p+1)
		case has(p, "#"):
			if has(p+1, "*") {
				return tok(tOPEN_BLOCK, p+2)
			}
			return tok(tOPEN_BLOCK, p+1)
		case has(p, "/"):
			return tok(tOPEN_ENDBLOCK, p+1)
		case has(p, "^"):
			if e := closeAt(p + 1); e >= 0 {
				return tok(tINVERSE, e)
			}
			return tok(tOPEN_INVERSE, p+1)
		}
		if q := skipSpace(src, p); has(q, "else") {
			if e := closeAt(q + 4); e >= 0 {
				return tok(tINVERSE, e)
			}
			return tok(tOPEN_INVERSE_CHAIN, q+4)
		}
		switch {
		case has(p, "{"):
			return tok(tOPEN_UNESCAPED, p+1)
		case has(p, "&"):
			return tok(tOPEN, p+1)
		case has(p, "!--"):
			return muComment, "", pos
		case has(p, "!"):
			if i := strings.Index(src[p+1:], "}}"); i >= 0 {
				return tok(tCOMMENT, p+1+i+2)
			}
		}
		if has(p, "*") {
			p++
		}
		return tok(tOPEN, p)
	case '}':
		if has(pos, "}}}}") {
			return tok(tCLOSE_RAW_BLOCK, pos+4)
		}
	case '=':
		return tok(tEQUALS, pos+1)
	case '.':
		if has(pos, "..") {
			return tok(tID, pos+2)
		}
		if lookahead(src, pos+1, laID, true) {
			return tok(tID, pos+1)
		}
		return tok(tSEP, pos+1)
	case '/':
		return tok(tSEP, pos+1)
	case '"', '\'':
		if e := scanQuoted(src, pos, c); e >= 0 {
			s := src[pos+1 : e-1]
			if strings.Contains(s, `\`+string(c)) {
				s = strings.ReplaceAll(s, `\`+string(c), string(c))
			}
			return tSTRING, s, e
		}
	case '@':
		return tok(tDATA, pos+1)
	case '|':
		return tok(tCLOSE_BLOCK_PARAMS, pos+1)
	}
	if w := spaceAt(src, pos); w > 0 {
		return muSkip, "", skipSpace(src, pos)
	}
	if c == '}' || c == '~' {
		i := pos
		if c == '}' { // CLOSE_UNESCAPED: "}" ~? "}}"
			i++
			if has(i, "~") {
				i++
			}
			if has(i, "}}") {
				return tok(tCLOSE_UNESCAPED, i+2)
			}
			i = pos
		} else {
			i++
		}
		if has(i, "}}") { // CLOSE: ~? "}}"
			return tok(tCLOSE, i+2)
		}
	}
	for _, l := range [...]struct {
		w string
		k tokenKind
	}{{"true", tBOOLEAN}, {"false", tBOOLEAN}, {"undefined", tUNDEFINED}, {"null", tNULL}} {
		if has(pos, l.w) && lookaheadSp(src, pos+len(l.w)) {
			return tok(l.k, pos+len(l.w))
		}
	}
	if c == '-' || (c >= '0' && c <= '9') { // NUMBER
		i := pos
		if c == '-' {
			i++
		}
		d := i
		for i < len(src) && src[i] >= '0' && src[i] <= '9' {
			i++
		}
		if i > d {
			if i+1 < len(src) && src[i] == '.' && src[i+1] >= '0' && src[i+1] <= '9' {
				j := i + 1
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
				if lookaheadSp(src, j) {
					return tok(tNUMBER, j)
				}
			}
			if lookaheadSp(src, i) {
				return tok(tNUMBER, i)
			}
		}
	}
	if has(pos, "as") {
		if i := skipSpace(src, pos+2); i > pos+2 && has(i, "|") {
			return tok(tOPEN_BLOCK_PARAMS, i+1)
		}
	}
	// ID: run of id chars followed by a LOOKAHEAD char
	i := pos
	for i < len(src) {
		r, w := utf8.DecodeRuneInString(src[i:])
		if !idRune(r) {
			break
		}
		i += w
	}
	if i > pos && lookahead(src, i, laID, true) {
		return tok(tID, i)
	}
	if c == '[' { // [segment] ID with \\ and \] escapes
		if e := scanQuoted(src, pos, ']'); e >= 0 {
			s := src[pos:e]
			if strings.IndexByte(s, '\\') >= 0 {
				var b strings.Builder
				for j := 0; j < len(s); j++ {
					if s[j] == '\\' && j+1 < len(s) && (s[j+1] == '\\' || s[j+1] == ']') {
						j++
					}
					b.WriteByte(s[j])
				}
				s = b.String()
			}
			return tID, s, e
		}
	}
	_, w := utf8.DecodeRuneInString(src[pos:])
	return tok(tINVALID, pos+w)
}

func lookaheadSp(s string, i int) bool { return lookahead(s, i, laLit, true) }

func itoa(n int) string { return strconv.Itoa(n) }
