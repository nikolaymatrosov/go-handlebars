package handlebars

import "strconv"

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
	inverted                            bool       // {{^x}}: the parser swapped program and inverse, so inverse comes first in the source
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
	// legacy is the parse-time string compiler.js pushParam makes of this
	// path as a param: under StringParams the string passed instead of its
	// value; under TrackIDs its id, or for a block param the child path
	// ("a.b" of x.a.b) appended to the param's id. "" when both are off.
	legacy string
}

type (
	strLit struct {
		loc
		value  string
		boxed  any    // value, boxed once at parse time: as an argument it costs no allocation
		legacy string // StringParams: the string pushParam passes for it as a param
	}
	numLit struct {
		loc
		value float64
		boxed any // value, boxed once at parse time
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
