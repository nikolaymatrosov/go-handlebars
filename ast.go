package handlebars

import (
	"strconv"
	"strings"
)

// The AST mirrors the handlebars.js nodes built by handlebars.yy and
// compiler/helpers.js, after whitespace control has run. Statements and
// expressions are pointers implementing node.

// node is any statement or expression.
type node interface{ pos() (line, col int) }

// loc is the JS loc.start of a node: 1-based line, 0-based UTF-16 column.
type loc struct{ line, col int }

func (l loc) pos() (int, int) { return l.line, l.col }

// stripFlags records `~` on a tag: open is `{{~`, close is `~}}`.
type stripFlags struct{ open, close bool }

// program is a statement list (JS Program).
type program struct {
	body        []node
	blockParams []string // `as |a b|` names (raw ID text); nil when absent
	chained     bool     // inverse holding the single block of an `{{else foo}}` chain
	decorators  []node   // the decorator blocks/mustaches in body (JS fn.decorator), run by s.wrap

	// Set on the copy s.wrap returns once the decorators ran (JS prog):
	props   map[string]any  // what decorators set on JS fn or props; block helpers read options.fn.x
	inlines []inlinePartial // {{#*inline}} partials defined (name and prog only); run binds their scope
}

// isDecorator reports whether n is {{* x}} or {{#* x}}...{{/x}}.
func isDecorator(n node) bool {
	switch n := n.(type) {
	case *mustacheNode:
		return n.decorator
	case *blockNode:
		return n.decorator
	}
	return false
}

// contentNode is literal text (JS ContentStatement). Empty values are kept,
// as in JS; the renderer just writes value.
type contentNode struct {
	loc
	value         string // text after whitespace control
	original      string // source text; read by whitespace control
	leftStripped  bool   // whitespace-control bookkeeping
	rightStripped bool
}

// commentNode is {{! ... }} (JS CommentStatement).
type commentNode struct {
	loc
	value string
	strip stripFlags
}

// call is the helper-call shape shared by mustaches, blocks and subexpressions.
type call struct {
	path   node // *pathExpr or a literal
	params []node
	hash   []hashPair // nil when absent
	// Set by the parser (parser.classify) on mustaches, blocks and
	// subexpressions: callee is path after transformLiteralToPath.
	callee *pathExpr
	kind   callKind
}

type hashPair struct {
	key   string
	value node
}

// mustacheNode is {{x}} / {{{x}}} / {{&x}}, or a decorator {{* x}}
// (JS MustacheStatement / Decorator).
type mustacheNode struct {
	loc
	call
	escaped   bool
	decorator bool
	strip     stripFlags
}

// blockNode is {{#x}}, {{^x}}, an `{{else x}}` chain link, a raw block
// {{{{x}}}}, or a decorator block {{#* x}} (JS BlockStatement / DecoratorBlock).
// Either program may be nil ({{^x}} has only inverse).
type blockNode struct {
	loc
	call
	program, inverse                    *program
	decorator                           bool
	openStrip, inverseStrip, closeStrip stripFlags // whitespace-control input
}

// partialNode is {{> name}} or, when program != nil, a partial block
// {{#> name}}...{{/name}} (JS PartialStatement / PartialBlockStatement).
type partialNode struct {
	loc
	name       node // *pathExpr, literal, or *subExpr (dynamic partial)
	params     []node
	hash       []hashPair
	indent     string // leading whitespace of a standalone partial
	program    *program
	strip      stripFlags // opening tag
	closeStrip stripFlags // {{/name}} of a partial block
}

// subExpr is (helper args...) (JS SubExpression).
type subExpr struct {
	loc
	call
}

// pathExpr is a resolved path (JS PathExpression after preparePath).
type pathExpr struct {
	loc
	data     bool     // @-prefixed
	depth    int      // number of leading ../
	parts    []string // segments with this/./.. removed; empty means the context itself
	original string   // as written, with [] stripped, e.g. "../foo/bar"
	scoped   bool     // JS AST.helpers.scopedId: original =~ /^\.|this\b/
	// bp: parts[0] names a block param, JS blockParamIndex: bpDepth enclosing
	// programs out, position bpIdx in its `as |...|` list.
	bp             bool
	bpDepth, bpIdx int
}

type (
	strLit struct {
		loc
		value string
	}
	numLit struct {
		loc
		value float64
	}
	boolLit struct {
		loc
		value bool
	}
	undefinedLit struct{ loc }
	nullLit      struct{ loc }
)

// jsOriginal returns the JS `original` of a path/literal stringified as JS
// would, plus its JS type ('s' string, 'n' number, 'b' boolean, 'l' null,
// 'u' undefined) so callers can mimic `===`.
func jsOriginal(n node) (string, byte) {
	switch n := n.(type) {
	case *pathExpr:
		return n.original, 's'
	case *strLit:
		return n.value, 's'
	case *numLit:
		return string(appendJSNumber(nil, n.value)), 'n'
	case *boolLit:
		return strconv.FormatBool(n.value), 'b'
	case *nullLit:
		return "null", 'l'
	}
	return "undefined", 'u' // undefinedLit, subExpr (no original)
}

// printAST renders p exactly like handlebars.js compiler/printer.js,
// including its padding drift (Program decrements padding on exit).
func printAST(p *program) string {
	var pr printer
	return pr.program(p)
}

type printer struct{ padding int }

func (pr *printer) pad(s string) string {
	return strings.Repeat("  ", max(pr.padding, 0)) + s + "\n"
}

func (pr *printer) program(p *program) string {
	var b strings.Builder
	if p.blockParams != nil {
		b.WriteString(pr.pad("BLOCK PARAMS: [ " + strings.Join(p.blockParams, " ") + " ]"))
	}
	for _, n := range p.body {
		b.WriteString(pr.accept(n))
	}
	pr.padding--
	return b.String()
}

func (pr *printer) accept(n node) string {
	switch n := n.(type) {
	case *contentNode:
		return pr.pad("CONTENT[ '" + n.value + "' ]")
	case *commentNode:
		return pr.pad("{{! '" + n.value + "' }}")
	case *mustacheNode:
		if n.decorator {
			return pr.pad("{{ DIRECTIVE " + pr.call(&n.call) + " }}")
		}
		return pr.pad("{{ " + pr.call(&n.call) + " }}")
	case *blockNode:
		return pr.block(n)
	case *partialNode:
		return pr.partial(n)
	case *subExpr:
		return pr.call(&n.call)
	case *pathExpr:
		s := "PATH:" + strings.Join(n.parts, "/")
		if n.data {
			s = "@" + s
		}
		return s
	case *strLit:
		return `"` + n.value + `"`
	case *numLit:
		return "NUMBER{" + string(appendJSNumber(nil, n.value)) + "}"
	case *boolLit:
		return "BOOLEAN{" + strconv.FormatBool(n.value) + "}"
	case *undefinedLit:
		return "UNDEFINED"
	case *nullLit:
		return "NULL"
	}
	return ""
}

func (pr *printer) block(n *blockNode) string {
	out := "BLOCK:"
	if n.decorator {
		out = "DIRECTIVE BLOCK:"
	}
	out = pr.pad(out)
	pr.padding++
	out += pr.pad(pr.call(&n.call))
	if n.program != nil {
		out += pr.pad("PROGRAM:")
		pr.padding++
		out += pr.program(n.program)
		pr.padding--
	}
	if n.inverse != nil {
		if n.program != nil {
			pr.padding++
		}
		out += pr.pad("{{^}}")
		pr.padding++
		out += pr.program(n.inverse)
		pr.padding--
		if n.program != nil {
			pr.padding--
		}
	}
	pr.padding--
	return out
}

func (pr *printer) partial(n *partialNode) string {
	orig, _ := jsOriginal(n.name)
	content := "PARTIAL:" + orig
	if n.program != nil {
		content = "PARTIAL BLOCK:" + orig
	}
	if len(n.params) > 0 {
		content += " " + pr.accept(n.params[0])
	}
	if n.hash != nil {
		content += " " + pr.hash(n.hash)
	}
	if n.program != nil {
		content += " " + pr.pad("PROGRAM:")
		pr.padding++
		content += pr.program(n.program)
		pr.padding--
	}
	return pr.pad("{{> " + content + " }}")
}

func (pr *printer) call(c *call) string {
	params := make([]string, len(c.params))
	for i, p := range c.params {
		params[i] = pr.accept(p)
	}
	s := pr.accept(c.path) + " [" + strings.Join(params, ", ") + "]"
	if c.hash != nil {
		s += " " + pr.hash(c.hash)
	}
	return s
}

func (pr *printer) hash(h []hashPair) string {
	pairs := make([]string, len(h))
	for i, kv := range h {
		pairs[i] = kv.key + "=" + pr.accept(kv.value)
	}
	return "HASH{" + strings.Join(pairs, ", ") + "}"
}
