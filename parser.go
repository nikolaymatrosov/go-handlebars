package handlebars

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parse lexes and parses src, then applies whitespace control, like
// handlebars.js Handlebars.parse. Recursive descent over the handlebars.yy
// grammar; node construction ports compiler/helpers.js.
func parse(src string, opts Options) (*program, error) {
	limits := opts.Limits.withDefaults()
	toks, lexErr := lex(src, limits)
	if _, ok := lexErr.(*LimitError); ok {
		return nil, lexErr
	}
	if lexErr != nil {
		toks = append(toks, token{kind: tEOF, off: len(src)}) // stands for the bad text
	}
	p := &parser{src: src, toks: toks, maxDepth: limits.MaxParseDepth, opts: &opts}
	prog, err := p.program(setOf(tEOF), nil)
	if lexErr != nil {
		// JS lexes lazily and every JS error needs a lookahead token, so only
		// errors found before reaching the bad text beat the lexical error.
		if _, isLimit := err.(*LimitError); !isLimit && (err == nil || p.i == len(toks)-1) {
			return nil, lexErr
		}
	}
	if err != nil {
		return nil, err
	}
	if p.compileErr != nil {
		return nil, p.compileErr
	}
	(&wsControl{standalone: !opts.IgnoreStandalone}).program(prog)
	return prog, nil
}

type parser struct {
	src             string
	toks            []token // always ends with tEOF
	i               int
	depth, maxDepth int
	bps             [][]string // blockParams of the enclosing programs, innermost last
	opts            *Options
	compileErr      error // first compile-time (compiler.js) error, reported after a clean parse
}

// tokSet is a bitset of token kinds.
type tokSet uint64

func setOf(ks ...tokenKind) tokSet {
	var s tokSet
	for _, k := range ks {
		s |= 1 << k
	}
	return s
}

func (s tokSet) has(k tokenKind) bool { return s&(1<<k) != 0 }

var (
	stmtStart = setOf(tCOMMENT, tCONTENT, tOPEN_RAW_BLOCK, tOPEN_BLOCK, tOPEN_INVERSE,
		tOPEN, tOPEN_UNESCAPED, tOPEN_PARTIAL, tOPEN_PARTIAL_BLOCK)
	nameStart  = setOf(tID, tSTRING, tNUMBER, tBOOLEAN, tUNDEFINED, tNULL, tDATA)
	paramStart = nameStart | setOf(tOPEN_SEXPR)
	// pathFollow is the LALR lookahead set after a path's last ID. Jison
	// reduces the path (running preparePath) only on these tokens, so
	// "Invalid path" errors must not fire on anything else.
	pathFollow = setOf(tCLOSE_RAW_BLOCK, tCLOSE, tCLOSE_UNESCAPED, tOPEN_SEXPR, tCLOSE_SEXPR,
		tID, tOPEN_BLOCK_PARAMS, tSTRING, tNUMBER, tBOOLEAN, tUNDEFINED, tNULL, tDATA, tSEP)
	// jisonOrder is the grammar's terminal numbering, used to list expected tokens.
	jisonOrder = [...]tokenKind{tEOF, tCOMMENT, tCONTENT, tEND_RAW_BLOCK, tOPEN_RAW_BLOCK,
		tCLOSE_RAW_BLOCK, tOPEN_BLOCK, tCLOSE, tOPEN_INVERSE, tOPEN_INVERSE_CHAIN, tINVERSE,
		tOPEN_ENDBLOCK, tOPEN, tOPEN_UNESCAPED, tCLOSE_UNESCAPED, tOPEN_PARTIAL,
		tOPEN_PARTIAL_BLOCK, tOPEN_SEXPR, tCLOSE_SEXPR, tID, tEQUALS, tOPEN_BLOCK_PARAMS,
		tCLOSE_BLOCK_PARAMS, tSTRING, tNUMBER, tBOOLEAN, tUNDEFINED, tNULL, tDATA, tSEP}
)

func (p *parser) peek() token { return p.toks[p.i] }

// peek2 is the token after the current one (tEOF at the end).
func (p *parser) peek2() tokenKind {
	if p.i+1 < len(p.toks) {
		return p.toks[p.i+1].kind
	}
	return tEOF
}

func (p *parser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

// expect consumes a token of kind k or reports a parse error.
func (p *parser) expect(k tokenKind) (token, error) {
	if p.peek().kind != k {
		return token{}, p.unexpected(setOf(k))
	}
	return p.next(), nil
}

// enter counts one nesting level before the caller recurses.
func (p *parser) enter() error {
	p.depth++
	if p.depth > p.maxDepth {
		return &LimitError{What: "parse depth", Max: p.maxDepth}
	}
	return nil
}

// unexpected builds Jison's error for the current token. Like Jison, the
// line in the text is where the last consumed token ends and the loc is that
// token's start (1:0 before any token).
func (p *parser) unexpected(expected tokSet) error {
	la := p.toks[p.i]
	line, eLine, eCol := 1, 1, 0
	if p.i > 0 {
		last := p.toks[p.i-1]
		line, eLine, eCol = last.line+countLines(last.text), last.line, last.col
	}
	var names []string
	for _, k := range jisonOrder {
		if expected.has(k) {
			names = append(names, "'"+k.String()+"'")
		}
	}
	msg := "Parse error on line " + itoa(line) + ":\n" + showPosition(p.src, la) +
		"\nExpecting " + strings.Join(names, ", ") + ", got '" + la.kind.String() + "'"
	return &Error{Line: eLine, Column: eCol, Message: msg}
}

// countLines counts line terminators (\r\n, \r, \n) like Jison's yylineno.
func countLines(s string) int {
	n := strings.Count(s, "\n") + strings.Count(s, "\r")
	return n - strings.Count(s, "\r\n")
}

// showPosition mirrors Jison's lexer.showPosition (in runes, not UTF-16 units):
// up to 20 chars before the token, 20 from it, and a caret line.
func showPosition(src string, la token) string {
	past, pre := src[:la.off], ""
	if n := utf8.RuneCountInString(past); n > 20 {
		pre = "..."
		for ; n > 20; n-- {
			_, w := utf8.DecodeRuneInString(past)
			past = past[w:]
		}
	}
	pre = strings.ReplaceAll(pre+past, "\n", "")
	next := src[la.off:]
	for i, n := 0, 0; i < len(next); n++ {
		if n == 20 {
			next = next[:i]
			break
		}
		_, w := utf8.DecodeRuneInString(next[i:])
		i += w
	}
	if utf8.RuneCountInString(la.text) > 20 {
		next += "..."
	}
	next = strings.ReplaceAll(next, "\n", "")
	return pre + next + "\n" + strings.Repeat("-", utf8.RuneCountInString(pre)) + "^"
}

// program parses statement* and requires the next token to be in end.
func (p *parser) program(end tokSet, blockParams []string) (*program, error) {
	p.bps = append(p.bps, blockParams) // JS compiler options.blockParams.unshift
	defer func() { p.bps = p.bps[:len(p.bps)-1] }()
	prog := &program{}
	for stmtStart.has(p.peek().kind) {
		n, err := p.statement()
		if err != nil {
			return nil, err
		}
		prog.body = append(prog.body, n)
		if isDecorator(n) {
			prog.decorators = append(prog.decorators, n)
		}
	}
	if !end.has(p.peek().kind) {
		if len(prog.body) == 0 {
			end |= stmtStart
		} // else Jison has reduced the program and expects only end
		return nil, p.unexpected(end)
	}
	return prog, nil
}

func (p *parser) statement() (node, error) {
	t := p.peek()
	switch t.kind {
	case tCONTENT:
		p.next()
		return &contentNode{loc: at(t), value: t.text, original: t.text}, nil
	case tCOMMENT:
		p.next()
		return &commentNode{loc: at(t), value: stripComment(t.text), strip: strip(t.text, t.text)}, nil
	case tOPEN, tOPEN_UNESCAPED:
		return p.mustache()
	case tOPEN_RAW_BLOCK:
		return p.rawBlock()
	case tOPEN_PARTIAL, tOPEN_PARTIAL_BLOCK:
		return p.partial()
	}
	return p.block() // tOPEN_BLOCK, tOPEN_INVERSE
}

func at(t token) loc { return loc{t.line, t.col} }

// strip is JS stripFlags: `~` right after "{{" of open, right before "}}" of close.
func strip(open, close string) stripFlags {
	return stripFlags{
		open:  len(open) > 2 && open[2] == '~',
		close: len(close) >= 3 && close[len(close)-3] == '~',
	}
}

// stripComment removes /^\{\{~?!-?-?/ and /-?-?~?\}\}$/.
func stripComment(s string) string {
	s = strings.TrimPrefix(s, "{{")
	s = strings.TrimPrefix(s, "~")
	s = strings.TrimPrefix(s, "!")
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimSuffix(s, "}}")
	s = strings.TrimSuffix(s, "~")
	s = strings.TrimSuffix(s, "-")
	return strings.TrimSuffix(s, "-")
}

// id strips [brackets] from an ID token, as JS id() does with /^\[.*\]$/
// (where . excludes line terminators).
func id(s string) string {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' || strings.ContainsAny(s, "\n\r\u2028\u2029") {
		return s
	}
	return s[1 : len(s)-1]
}

func (p *parser) mustache() (node, error) {
	open := p.next()
	closeKind := tCLOSE
	if open.kind == tOPEN_UNESCAPED {
		closeKind = tCLOSE_UNESCAPED
	}
	c, err := p.call(false, setOf(closeKind))
	if err != nil {
		return nil, err
	}
	close := p.next()
	// JS: escapeFlag = open.charAt(3) || open.charAt(2)
	flag := byte(0)
	if len(open.text) > 3 {
		flag = open.text[3]
	} else if len(open.text) > 2 {
		flag = open.text[2]
	}
	n := &mustacheNode{
		loc:       at(open),
		call:      c,
		escaped:   flag != '{' && flag != '&',
		decorator: strings.Contains(open.text, "*"),
		strip:     strip(open.text, close.text),
	}
	if !n.decorator {
		p.classify(n, &n.call, false)
	}
	return n, nil
}

// call parses `helperName param* hash?` (partialName allows a subexpression)
// and requires the next token to be in end, which it leaves unconsumed.
func (p *parser) call(partialName bool, end tokSet) (call, error) {
	var c call
	var err error
	if partialName && p.peek().kind == tOPEN_SEXPR {
		c.path, err = p.sexpr()
	} else if partialName {
		c.path, err = p.helperName(paramStart)
	} else {
		c.path, err = p.helperName(nameStart)
	}
	if err != nil {
		return c, err
	}
	for paramStart.has(p.peek().kind) && !(p.peek().kind == tID && p.peek2() == tEQUALS) {
		v, err := p.param()
		if err != nil {
			return c, err
		}
		c.params = append(c.params, v)
	}
	// Once a hash starts, every ID is a key (Jison shifts it and then wants EQUALS).
	for p.peek().kind == tID && (c.hash != nil || p.peek2() == tEQUALS) {
		key := p.next()
		if _, err := p.expect(tEQUALS); err != nil {
			return c, err
		}
		v, err := p.param()
		if err != nil {
			return c, err
		}
		c.hash = append(c.hash, hashPair{id(key.text), v})
	}
	if !end.has(p.peek().kind) {
		if c.hash == nil {
			end |= paramStart
		} // else Jison has reduced the hash and expects only end
		return c, p.unexpected(end)
	}
	return c, nil
}

func (p *parser) param() (node, error) {
	if p.peek().kind == tOPEN_SEXPR {
		return p.sexpr()
	}
	return p.helperName(paramStart)
}

func (p *parser) sexpr() (node, error) {
	open := p.next()
	if err := p.enter(); err != nil {
		return nil, err
	}
	c, err := p.call(false, setOf(tCLOSE_SEXPR))
	if err != nil {
		return nil, err
	}
	p.next()
	p.depth--
	n := &subExpr{loc: at(open), call: c}
	p.classify(n, &n.call, true)
	return n, nil
}

// helperName parses a path, data path or literal; expected is reported on error.
func (p *parser) helperName(expected tokSet) (node, error) {
	t := p.peek()
	switch t.kind {
	case tID:
		return p.path(t, false)
	case tDATA:
		p.next()
		return p.path(t, true)
	case tSTRING:
		p.next()
		n := &strLit{loc: at(t), value: t.text}
		if p.opts.StringParams {
			n.legacy = stringParam(t.text)
		}
		return n, nil
	case tNUMBER:
		p.next()
		f, _ := strconv.ParseFloat(t.text, 64) // lexer guarantees -?\d+(\.\d+)?; overflow gives ±Inf like JS
		return &numLit{at(t), f}, nil
	case tBOOLEAN:
		p.next()
		return &boolLit{at(t), t.text == "true"}, nil
	case tUNDEFINED:
		p.next()
		return &undefinedLit{at(t)}, nil
	case tNULL:
		p.next()
		return &nullLit{at(t)}, nil
	}
	return nil, p.unexpected(expected)
}

// path parses `ID (SEP ID)*` (after DATA when data) and ports preparePath.
// start is the path's first token (DATA or ID), which gives its loc.
func (p *parser) path(start token, data bool) (node, error) {
	pe := &pathExpr{loc: at(start), data: data}
	var orig strings.Builder
	if data {
		orig.WriteByte('@')
	}
	invalid := ""
	for sep := ""; ; {
		t, err := p.expect(tID)
		if err != nil {
			return nil, err
		}
		part := id(t.text)
		orig.WriteString(sep)
		orig.WriteString(part)
		if part == t.text && (part == ".." || part == "." || part == "this") {
			if len(pe.parts) > 0 {
				if invalid == "" {
					invalid = orig.String()
				}
			} else if part == ".." {
				pe.depth++
			}
		} else {
			pe.parts = append(pe.parts, part)
		}
		if p.peek().kind != tSEP {
			break
		}
		sep = p.next().text
	}
	if !pathFollow.has(p.peek().kind) {
		return nil, p.unexpected(pathFollow)
	}
	if invalid != "" {
		return nil, &Error{Line: pe.line, Column: pe.col, Message: "Invalid path: " + invalid}
	}
	pe.original = orig.String()
	pe.scoped = isScoped(pe.original)
	if !data && pe.depth == 0 && !pe.scoped && len(pe.parts) > 0 {
		pe.bpDepth, pe.bpIdx, pe.bp = p.blockParamIndex(pe.parts[0])
	}
	switch { // compiler.js pushParam; stringParams wins, as there
	case p.opts.StringParams:
		pe.legacy = stringParam(pe.original)
	case p.opts.TrackIDs && pe.bp: // pushId('BlockParam', ..., parts.slice(1).join('.'))
		pe.legacy = strings.Join(pe.parts[1:], ".")
	case p.opts.TrackIDs: // original.replace(/^this(?:\.|$)/, '').replace(/^\.\//, '').replace(/^\.$/, '')
		s := pe.original
		if strings.HasPrefix(s, "this") && (len(s) == 4 || s[4] == '.') {
			s = s[min(5, len(s)):]
		}
		if s = strings.TrimPrefix(s, "./"); s == "." {
			s = ""
		}
		pe.legacy = s
	}
	return pe, nil
}

// stringParam is compiler.js pushParam's stringParams form of a string
// value (a path's original or a string literal; numbers and booleans have
// no .replace): value.replace(/^(\.?\.\/)*/g, "").replace(/\//g, ".").
func stringParam(s string) string {
	for strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		s = s[strings.IndexByte(s, '/')+1:]
	}
	return strings.ReplaceAll(s, "/", ".")
}

// blockParamIndex is compiler.js blockParamIndex: the (program depth, index)
// of the nearest enclosing `as |name|`, depth 0 being the innermost program.
func (p *parser) blockParamIndex(name string) (depth, idx int, ok bool) {
	for d := 0; d < len(p.bps); d++ {
		if i := slices.Index(p.bps[len(p.bps)-1-d], name); i >= 0 {
			return d, i, true
		}
	}
	return 0, 0, false
}

// isScoped is JS AST.helpers.scopedId: /^\.|this\b/ on a path's original.
func isScoped(original string) bool {
	return strings.HasPrefix(original, ".") || hasThisWord(original)
}

// hasThisWord reports whether s matches /this\b/.
func hasThisWord(s string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], "this")
		if j < 0 {
			return false
		}
		if i += j + 4; i == len(s) || !isWordByte(s[i]) {
			return true
		}
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// openTag is a parsed block opener (openBlock/openInverse/openInverseChain).
type openTag struct {
	tok         token
	call        call
	blockParams []string
	strip       stripFlags
}

func (p *parser) openTag() (openTag, error) {
	o := openTag{tok: p.next()}
	var err error
	if o.call, err = p.call(false, setOf(tCLOSE, tOPEN_BLOCK_PARAMS)); err != nil {
		return o, err
	}
	if p.peek().kind == tOPEN_BLOCK_PARAMS {
		p.next()
		for {
			t, err := p.expect(tID)
			if err != nil {
				return o, err
			}
			// JS applies id() to the whole array, which leaves the names raw.
			o.blockParams = append(o.blockParams, t.text)
			if p.peek().kind != tID {
				break
			}
		}
		if p.peek().kind != tCLOSE_BLOCK_PARAMS {
			return o, p.unexpected(setOf(tID, tCLOSE_BLOCK_PARAMS))
		}
		p.next()
	}
	close, err := p.expect(tCLOSE)
	if err != nil {
		return o, err
	}
	o.strip = strip(o.tok.text, close.text)
	return o, nil
}

// inverseInfo is JS inverseAndProgram / inverseChain: the tag's strip, the
// inverse program and, for an `{{else x}}` chain, the block inside it.
type inverseInfo struct {
	strip   stripFlags
	program *program
	chain   *blockNode
}

// block parses {{#x}}...{{/x}} and {{^x}}...{{/x}}.
func (p *parser) block() (node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	open, err := p.openTag()
	if err != nil {
		return nil, err
	}
	inverted := open.tok.kind == tOPEN_INVERSE
	end := setOf(tINVERSE, tOPEN_ENDBLOCK)
	if !inverted {
		end |= setOf(tOPEN_INVERSE_CHAIN)
	}
	prog, err := p.program(end, open.blockParams)
	if err != nil {
		return nil, err
	}
	var inv *inverseInfo
	if k := p.peek().kind; k == tINVERSE {
		inv, err = p.inverseAndProgram()
	} else if k == tOPEN_INVERSE_CHAIN {
		inv, err = p.inverseChain()
	}
	if err != nil {
		return nil, err
	}
	closeStrip, err := p.closeBlock(open.call.path)
	if err != nil {
		return nil, err
	}
	p.depth--
	return p.prepareBlock(open, prog, inv, closeStrip, inverted)
}

// closeBlock parses {{/name}} and checks it against the opening name.
func (p *parser) closeBlock(openName node) (stripFlags, error) {
	open, err := p.expect(tOPEN_ENDBLOCK)
	if err != nil {
		return stripFlags{}, err
	}
	name, err := p.helperName(nameStart)
	if err != nil {
		return stripFlags{}, err
	}
	close, err := p.expect(tCLOSE)
	if err != nil {
		return stripFlags{}, err
	}
	return strip(open.text, close.text), validateClose(openName, name)
}

// inverseAndProgram parses `{{else}}` / `{{^}}` and its program.
func (p *parser) inverseAndProgram() (*inverseInfo, error) {
	t := p.next()
	prog, err := p.program(setOf(tOPEN_ENDBLOCK), nil)
	if err != nil {
		return nil, err
	}
	return &inverseInfo{strip: strip(t.text, t.text), program: prog}, nil
}

// inverseChain parses `{{else x}} program inverseChain?`; each link nests.
func (p *parser) inverseChain() (*inverseInfo, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	// The chained block lives in a wrapper inverse program: one more level.
	p.bps = append(p.bps, nil)
	defer func() { p.bps = p.bps[:len(p.bps)-1] }()
	open, err := p.openTag()
	if err != nil {
		return nil, err
	}
	prog, err := p.program(setOf(tINVERSE, tOPEN_INVERSE_CHAIN, tOPEN_ENDBLOCK), open.blockParams)
	if err != nil {
		return nil, err
	}
	var next *inverseInfo
	if k := p.peek().kind; k == tINVERSE {
		next, err = p.inverseAndProgram()
	} else if k == tOPEN_INVERSE_CHAIN {
		next, err = p.inverseChain()
	}
	if err != nil {
		return nil, err
	}
	var closeStrip stripFlags // JS passes the nested chain as `close`
	if next != nil {
		closeStrip = next.strip
	}
	b, err := p.prepareBlock(open, prog, next, closeStrip, false)
	if err != nil {
		return nil, err
	}
	p.depth--
	return &inverseInfo{strip: open.strip, program: &program{body: []node{b}, chained: true}, chain: b}, nil
}

// prepareBlock ports helpers.js prepareBlock (validateClose is done by the caller).
func (p *parser) prepareBlock(open openTag, prog *program, inv *inverseInfo, closeStrip stripFlags, inverted bool) (*blockNode, error) {
	b := &blockNode{
		loc:        at(open.tok),
		call:       open.call,
		decorator:  strings.Contains(open.tok.text, "*"),
		openStrip:  open.strip,
		closeStrip: closeStrip,
	}
	prog.blockParams = open.blockParams
	if inv != nil {
		if b.decorator {
			return nil, &Error{Message: "Unexpected inverse block on decorator"} // JS passes no loc
		}
		if inv.chain != nil {
			inv.chain.closeStrip = closeStrip
		}
		b.inverseStrip = inv.strip
		b.inverse = inv.program
	}
	b.program = prog
	if inverted {
		b.program, b.inverse = b.inverse, b.program
	}
	if !b.decorator {
		p.classify(b, &b.call, false)
	}
	return b, nil
}

// validateClose ports helpers.js validateClose: open and close originals must
// be === (a raw block's close is the END_RAW_BLOCK name, a string).
func validateClose(open, close node) error {
	o, ot := jsOriginal(open)
	c, ct := jsOriginal(close)
	if o == c && ot == ct {
		return nil
	}
	line, col := open.pos()
	return &Error{Line: line, Column: col, Message: o + " doesn't match " + c}
}

// rawBlock parses {{{{x}}}}raw content{{{{/x}}}}.
func (p *parser) rawBlock() (node, error) {
	open := p.next()
	if err := p.enter(); err != nil {
		return nil, err
	}
	c, err := p.call(false, setOf(tCLOSE_RAW_BLOCK))
	if err != nil {
		return nil, err
	}
	p.next()
	prog := &program{}
	for p.peek().kind == tCONTENT {
		t := p.next()
		prog.body = append(prog.body, &contentNode{loc: at(t), value: t.text, original: t.text})
	}
	if p.peek().kind != tEND_RAW_BLOCK {
		return nil, p.unexpected(setOf(tCONTENT, tEND_RAW_BLOCK))
	}
	end := p.next()
	if err := validateClose(c.path, &strLit{value: end.text}); err != nil {
		return nil, err
	}
	p.depth--
	n := &blockNode{loc: at(open), call: c, program: prog}
	p.classify(n, &n.call, false)
	return n, nil
}

// partial parses {{> name ...}} and {{#> name ...}}...{{/name}}.
func (p *parser) partial() (node, error) {
	open := p.peek()
	isBlock := open.kind == tOPEN_PARTIAL_BLOCK
	if isBlock {
		if err := p.enter(); err != nil {
			return nil, err
		}
	}
	p.next()
	c, err := p.call(true, setOf(tCLOSE))
	if err != nil {
		return nil, err
	}
	close := p.next()
	n := &partialNode{loc: at(open), name: c.path, params: c.params, hash: c.hash, strip: strip(open.text, close.text)}
	if len(n.params) > 1 && p.compileErr == nil { // compiler.js PartialStatement
		p.compileErr = errAt(n, "Unsupported number of partial arguments: "+itoa(len(n.params)))
	}
	if !isBlock {
		return n, nil
	}
	if n.program, err = p.program(setOf(tOPEN_ENDBLOCK), nil); err != nil {
		return nil, err
	}
	if n.closeStrip, err = p.closeBlock(c.path); err != nil {
		return nil, err
	}
	p.depth--
	return n, nil
}
