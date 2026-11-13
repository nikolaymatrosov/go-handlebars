package handlebars

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
)

// Helper dispatch follows handlebars.js: the compiler (compiler.js
// classifySexpr/helperSexpr) decides at parse time how each mustache, block
// and subexpression is called; the renderer then runs the matching
// javascript-compiler.js opcode with the runtime.js hooks.

// Helper implements a handlebars helper.
type Helper func(opts *HelperOptions) (any, error)

// HelperOptions carries evaluated arguments and block closures. It is valid
// only during the helper call. Retaining it is unsupported, but it never
// touches other renders: a render that handed one to user code doesn't
// recycle its state. The render methods of a zero HelperOptions, which no
// render passed, return an error.
type HelperOptions struct {
	Name string         // helper name as written in the template
	Args []any          // evaluated positional args
	Hash map[string]any // evaluated hash args; nil when the call has none

	// Legacy is what StringParams or TrackIDs add (JS options.types, ids,
	// ...) to a call that evaluates arguments (a helper with params or
	// hash, a subexpression, a decorator) and, under TrackIDs, to the each
	// call a bare block over an array makes ({{#items}}: IDs is
	// ["items"]). nil when both options are off and for other bare
	// {{name}} and {{#name}} calls.
	Legacy *LegacyParams

	s           *renderState
	this        any      // JS `this`
	fn, inverse *program // block programs; nil = none (container.noop)
	argv        [2]any   // backs Args of up to two values: one allocation per call, not two
}

// argSlice is an Args of n values, in o.argv when they fit.
func (o *HelperOptions) argSlice(n int) []any {
	if n <= len(o.argv) {
		return o.argv[:n:n]
	}
	return make([]any, n)
}

// LegacyParams are what the legacy compile options add to a helper's
// options (compiler.js setupParams).
type LegacyParams struct {
	// Under StringParams a path argument is passed as its source string
	// ("../a/b" as "a.b", undefined/null as ""). Types[i] is the JS AST type
	// of argument i ("PathExpression", "StringLiteral", "NumberLiteral",
	// "BooleanLiteral", "UndefinedLiteral", "NullLiteral", "SubExpression");
	// Contexts[i] is the context its path is relative to.
	Types        []string
	HashTypes    map[string]string
	Contexts     []any
	HashContexts map[string]any
	// Under TrackIDs (when StringParams is off), IDs[i] is JS
	// options.ids[i] as a string: a path argument's path ("" for this), a
	// block param's recorded path, "null" for a literal and "true" for a
	// subexpression.
	IDs     []string
	HashIDs map[string]string
}

// id0 is JS options.ids[0]; ok is false when options.ids is unset.
func (o *HelperOptions) id0() (id string, ok bool) {
	if o.Legacy == nil || len(o.Legacy.IDs) == 0 {
		return "", false
	}
	return o.Legacy.IDs[0], true
}

// Ctx is the helper's `this`: the current context, or an empty object when
// that is null/undefined (JS container.nullContext).
func (o *HelperOptions) Ctx() any { return o.this }

// IsBlock reports whether the helper was called as a block ({{#name}}),
// so Fn or Inverse has something to render.
func (o *HelperOptions) IsBlock() bool { return o.fn != nil || o.inverse != nil }

// Fn renders the block with the helper's `this` (JS options.fn(this)).
func (o *HelperOptions) Fn() (string, error) { return o.FnWith(o.this) }

// FnWith renders the block with ctx (JS options.fn(ctx)).
func (o *HelperOptions) FnWith(ctx any) (string, error) {
	if o.s == nil {
		return "", errNoRender
	}
	return o.s.renderString(o.fn, ctx)
}

// FnTo renders the block with ctx and block params into w (JS
// options.fn(ctx, {blockParams, data})). A non-nil dataVars adds a child
// of the current @data frame holding dataVars (what helpers get from
// createFrame(options.data) plus assignments), so inherited variables stay
// visible; nil keeps the current frame. A JS bare {data: obj} that
// replaces the frame wholesale is not expressible (the corpus never needs
// it). The bytes count against MaxOutputSize;
// w is the helper's own sink, not the template's output.
func (o *HelperOptions) FnTo(w io.Writer, ctx any, blockParams []any, dataVars map[string]any) error {
	return o.FnToPaths(w, ctx, blockParams, nil, dataVars)
}

// FnToPaths is FnTo plus the block params' source paths (JS
// blockParams.path): under TrackIDs an argument rooted at block param i
// gets paths[i] as its id ("undefined" when missing).
func (o *HelperOptions) FnToPaths(w io.Writer, ctx any, blockParams []any, paths []string, dataVars map[string]any) error {
	if o.s == nil {
		return errNoRender
	}
	return o.s.renderTo(w, o.fn, ctx, bpFrame{vals: blockParams, ids: paths}, o.s.newFrame(dataVars))
}

// Inverse renders the {{else}} block with the helper's `this`.
func (o *HelperOptions) Inverse() (string, error) {
	if o.s == nil {
		return "", errNoRender
	}
	return o.s.renderString(o.inverse, o.this)
}

// InverseTo renders the {{else}} block with ctx into w (JS options.inverse(ctx)).
func (o *HelperOptions) InverseTo(w io.Writer, ctx any) error {
	if o.s == nil {
		return errNoRender
	}
	return o.s.renderTo(w, o.inverse, ctx, bpFrame{}, nil)
}

// errNoRender is what the render methods of a HelperOptions that no render
// passed to a helper (a zero value) return.
var errNoRender = errors.New("handlebars: HelperOptions is not from a render")

// DataVar is the @name data variable visible to the helper.
func (o *HelperOptions) DataVar(name string) any {
	if o.s == nil || o.s.data == nil {
		return nil
	}
	v, _ := o.s.data.get(name)
	return v
}

// Lookup resolves a dot-separated property path ("a.b") from Ctx(), one JS
// options.lookupProperty per part. Function values are returned, not called.
// path is a property chain only: ../, this and [] segments are not parsed.
func (o *HelperOptions) Lookup(path string) any {
	v := o.this
	for {
		part, rest, more := strings.Cut(path, ".")
		if isNull(v) {
			return nil
		}
		v, _ = lookupProperty(v, part)
		if !more {
			return v
		}
		path = rest
	}
}

// BlockParamNames are the block's `as |a b|` names (JS options.fn.blockParams
// is their count); nil when it declares none.
func (o *HelperOptions) BlockParamNames() []string {
	if o.fn == nil {
		return nil
	}
	return slices.Clone(o.fn.blockParams)
}

// callKind is how a call is dispatched, decided at parse time by classify.
type callKind uint8

const (
	kindSimple    callKind = iota // resolvePossibleLambda (blocks: blockValue)
	kindAmbiguous                 // invokeAmbiguous (blocks: ambiguousBlockValue)
	kindHelper                    // invokeHelper
	kindKnown                     // invokeKnownHelper
)

// classify ports compiler.js classifySexpr plus the knownHelpers branch of
// helperSexpr. It returns the knownHelpersOnly compile error, if any.
func (o *Options) classify(p *pathExpr, c *call, sub bool) (callKind, string) {
	simple := len(p.parts) == 1 && !p.scoped && p.depth == 0
	if simple && p.bp { // isBlockParam: never a helper
		return kindSimple, ""
	}
	name := helperName(p)
	if sub || len(c.params) > 0 || c.hash != nil { // AST.helpers.helperExpression
		switch {
		case o.known(name):
			return kindKnown, ""
		case o.KnownHelpersOnly:
			return kindHelper, "You specified knownHelpersOnly, but used the unknown helper " + name
		}
		return kindHelper, ""
	}
	switch {
	case !simple:
		return kindSimple, ""
	case o.known(name):
		return kindKnown, ""
	case o.KnownHelpersOnly:
		return kindSimple, ""
	}
	return kindAmbiguous, ""
}

// classify resolves and classifies the call of n (a mustache, block or
// subexpression) the way compiler.js does when compiling it.
func (p *parser) classify(n node, c *call, sub bool) {
	pe := callPath(c.path)
	if _, isPath := c.path.(*pathExpr); !isPath && !pe.scoped { // a literal callee can name a block param
		pe.bpDepth, pe.bpIdx, pe.bp = p.blockParamIndex(pe.parts[0])
	}
	c.callee = pe
	var msg string
	if c.kind, msg = p.opts.classify(pe, c, sub); msg != "" && p.compileErr == nil {
		p.compileErr = errAt(n, msg)
	}
}

// helperName is JS path.parts[0], the name helperSexpr and knownHelpers use.
func helperName(p *pathExpr) string {
	if len(p.parts) == 0 {
		return "undefined"
	}
	return p.parts[0]
}

// known is compiler.js options.knownHelpers[name]: the builtins, overridden
// by Options.KnownHelpers.
func (o *Options) known(name string) bool {
	if v, ok := o.KnownHelpers[name]; ok {
		return v
	}
	switch name {
	case "helperMissing", "blockHelperMissing", "each", "if", "unless", "with", "log", "lookup":
		return true
	}
	return false
}

// callPath is JS transformLiteralToPath: a literal callee ({{"foo"}},
// {{12}}, {{true}}) becomes a one-part path named by its JS string form.
func callPath(n node) *pathExpr {
	if p, ok := n.(*pathExpr); ok {
		return p
	}
	orig, _ := jsOriginal(n)
	line, col := n.pos()
	return &pathExpr{loc: loc{line, col}, parts: []string{orig}, original: orig, scoped: isScoped(orig)}
}

// helper is container.helpers[name]. runtime.js moves helperMissing and
// blockHelperMissing to container.hooks, so templates can't call them.
// builtin reports that h is a library builtin (called directly) rather
// than user code (called through s.call).
func (s *renderState) helper(name string) (h Helper, builtin bool) {
	if name == "helperMissing" || name == "blockHelperMissing" {
		return nil, false
	}
	if h = s.t.helpers[name]; h != nil {
		return h, false
	}
	h = builtinHelper(name)
	return h, h != nil
}

// callHelper runs a helper found by s.helper.
func (s *renderState) callHelper(name string, h Helper, builtin bool, o *HelperOptions) (any, error) {
	if builtin {
		return h(o)
	}
	return s.call(name, h, o)
}

// callHook runs container.hooks[name]: the registered override (user
// code, recovered) or the builtin (library code, called directly).
func (s *renderState) callHook(name string, builtin Helper, o *HelperOptions) (any, error) {
	if h := s.t.helpers[name]; h != nil {
		return s.call(name, h, o)
	}
	return builtin(o)
}

// ErrHelperPanic is wrapped by the error a render returns when a
// user-supplied function (a registered helper or hook, a function value in
// the data, a function partial or the Logger) panics; the error text
// carries the panic value and stack. Library code is never recovered, and
// neither are panics in a KeyedCollection or the writer passed to RenderTo,
// unless they happen while a user helper renders its block.
var ErrHelperPanic = errors.New("handlebars: helper panicked")

// call runs h, a user-supplied function, as name. A panic in it becomes an
// ErrHelperPanic error and the render state is restored, so the render
// fails cleanly even if the panic hit mid-block. Builtins must not use it.
// Every HelperOptions user code sees is passed here, so a non-nil o marks
// s escaped: user code may keep o, so s is never pooled.
func (s *renderState) call(name string, h Helper, o *HelperOptions) (v any, err error) {
	if o != nil {
		s.escaped = true
	}
	out, depths, bps, depth, data, inlines, opts := s.out, s.depths, s.bps, s.depth, s.data, s.inlines, s.opts
	defer func() {
		if r := recover(); r != nil {
			s.out, s.depths, s.bps, s.depth, s.data, s.inlines, s.opts = out, depths, bps, depth, data, inlines, opts
			v, err = nil, fmt.Errorf("%w (%s): %v\n%s", ErrHelperPanic, name, r, debug.Stack())
		}
	}()
	return h(o)
}

// expr evaluates a mustache, block or subexpression call per its parse-time
// kind. A block whose value is not a helper's result goes through the
// blockHelperMissing hook (blockValue / ambiguousBlockValue).
func (s *renderState) expr(c *call, ctx any, block bool, fn, inverse *program) (any, error) {
	if err := s.step(); err != nil {
		return nil, err
	}
	p := c.callee
	switch {
	case p == nil:
		return nil, errAt(c.path, "internal error: call not classified")
	case c.kind == kindHelper || c.kind == kindKnown:
		return s.invokeHelper(c, ctx, fn, inverse)
	case c.kind == kindSimple: // a function value is called with `this` = ctx and no options
		v, err := s.lookup(p, ctx, true)
		if err != nil {
			return nil, err
		}
		if f, ok := asHelper(v); ok {
			v, err = s.call(p.original, f, &HelperOptions{s: s, this: ctx})
		}
		if err != nil || !block {
			return v, err
		}
		o := &HelperOptions{Name: p.original, s: s, this: ctx, fn: fn, inverse: inverse}
		o.Args = o.argSlice(1)
		o.Args[0] = v
		return s.callHook("blockHelperMissing", blockHelperMissing, o)
	}
	// invokeAmbiguous: (helpers[name] || value), called if a function,
	// helperMissing if null.
	name := p.parts[0]
	h, builtin := s.helper(name)
	registered, missing := h != nil, false
	var v any
	if !registered {
		var err error
		if v, err = s.lookup(p, ctx, true); err != nil {
			return nil, err
		}
		h, _ = asHelper(v)
		missing = h == nil && isNull(v) && !s.opts.Strict // strict drops helperMissing
	}
	var o *HelperOptions
	if h != nil || missing {
		o = &HelperOptions{Name: name, s: s, this: helperThis(ctx), fn: fn, inverse: inverse}
		var err error
		if missing {
			v, err = s.callHook("helperMissing", helperMissing, o)
		} else {
			v, err = s.callHelper(name, h, builtin, o)
		}
		if err != nil || registered {
			return v, err
		}
	}
	if !block {
		return v, nil
	}
	if o == nil {
		o = &HelperOptions{Name: name, s: s, fn: fn, inverse: inverse}
	}
	o.Args, o.this = o.argSlice(1), ctx // ambiguousBlockValue reuses the options
	o.Args[0] = v
	return s.callHook("blockHelperMissing", blockHelperMissing, o)
}

// invokeHelper is JS helperSexpr: invokeKnownHelper,
// helpers[name].call(this, ...args, options), or invokeHelper,
// (helpers[name] || nonHelper || helperMissing).call(this, ...args, options).
func (s *renderState) invokeHelper(c *call, ctx any, fn, inverse *program) (any, error) {
	p := c.callee
	o := s.freeOpts.get()
	*o = HelperOptions{Name: p.original, s: s, this: helperThis(ctx), fn: fn, inverse: inverse}
	if err := s.args(c, ctx, o); err != nil {
		return nil, err
	}
	if c.kind == kindKnown {
		o.Name = helperName(p)
		h, builtin := s.helper(o.Name)
		if h == nil { // JS: TypeError, helpers[name] is undefined
			return nil, &Error{Message: "Missing helper: \"" + o.Name + "\""}
		}
		return s.callOwned(h, builtin, o)
	}
	var h Helper
	builtin := false
	if len(p.parts) == 1 && !p.scoped && p.depth == 0 { // AST.helpers.simpleId
		h, builtin = s.helper(p.parts[0])
	}
	if h == nil {
		nonHelper, err := s.lookup(p, ctx, true)
		if err != nil {
			return nil, err
		}
		var ok bool
		if h, ok = asHelper(nonHelper); !ok {
			if s.opts.Strict || jsTruthy(nonHelper) { // JS: TypeError, calling a non-function
				return nil, errAt(c.path, p.original+" is not a function")
			}
			return s.callHook("helperMissing", helperMissing, o)
		}
	}
	return s.callOwned(h, builtin, o)
}

// callOwned is callHelper for invokeHelper's own options, which it
// recycles once a builtin is done with them. User code may keep them, so
// those are left to the GC.
func (s *renderState) callOwned(h Helper, builtin bool, o *HelperOptions) (any, error) {
	v, err := s.callHelper(o.Name, h, builtin, o)
	if builtin {
		s.freeOpts.put(o)
	}
	return v, err
}

// args evaluates c's params and hash into o.Args and o.Hash (JS setupParams).
func (s *renderState) args(c *call, ctx any, o *HelperOptions) error {
	o.Args = o.argSlice(len(c.params))
	for i, n := range c.params {
		v, err := s.param(n, ctx)
		if err != nil {
			return err
		}
		o.Args[i] = v
	}
	if c.hash != nil {
		o.Hash = make(map[string]any, len(c.hash))
		for _, kv := range c.hash {
			v, err := s.param(kv.value, ctx)
			if err == nil {
				err = s.steps(len(kv.key) >> 10)
			}
			if err != nil {
				return err
			}
			o.Hash[kv.key] = v
		}
	}
	switch {
	case s.opts.StringParams:
		s.stringParams(c, ctx, o)
	case s.opts.TrackIDs:
		return s.trackIDs(c, o)
	}
	return nil
}

// stringParams sets o's types and contexts (JS setupParams under
// stringParams: pushStringParam's type and getContext(val.depth || 0)).
func (s *renderState) stringParams(c *call, ctx any, o *HelperOptions) {
	l := &LegacyParams{Types: make([]string, len(c.params)), Contexts: make([]any, len(c.params))}
	for i, n := range c.params {
		l.Types[i], l.Contexts[i] = nodeType(n), s.paramContext(n, ctx)
	}
	if c.hash != nil {
		l.HashTypes, l.HashContexts = make(map[string]string, len(c.hash)), make(map[string]any, len(c.hash))
		for _, kv := range c.hash {
			l.HashTypes[kv.key], l.HashContexts[kv.key] = nodeType(kv.value), s.paramContext(kv.value, ctx)
		}
	}
	o.Legacy = l
}

// paramContext is JS depths[val.depth] (depth0 for 0) for param n.
func (s *renderState) paramContext(n node, ctx any) any {
	p, ok := n.(*pathExpr)
	if !ok || p.depth == 0 {
		return ctx
	}
	if i := len(s.depths) - 1 - p.depth; i >= 0 {
		return s.depths[i]
	}
	return nil
}

// nodeType is the JS AST type of param n.
func nodeType(n node) string {
	switch n.(type) {
	case *pathExpr:
		return "PathExpression"
	case *subExpr:
		return "SubExpression"
	case *strLit:
		return "StringLiteral"
	case *numLit:
		return "NumberLiteral"
	case *boolLit:
		return "BooleanLiteral"
	case *undefinedLit:
		return "UndefinedLiteral"
	}
	return "NullLiteral"
}

// trackIDs sets o's ids (JS setupParams under trackIds).
func (s *renderState) trackIDs(c *call, o *HelperOptions) error {
	l := &LegacyParams{IDs: make([]string, len(c.params))}
	o.Legacy = l
	var err error
	for i, n := range c.params {
		if l.IDs[i], err = s.paramID(n); err != nil {
			return err
		}
	}
	if c.hash != nil {
		l.HashIDs = make(map[string]string, len(c.hash))
		for _, kv := range c.hash {
			if l.HashIDs[kv.key], err = s.paramID(kv.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// paramID is javascript-compiler.js pushId for param n, as a string: a
// path's parse-time id; for a block param, its frame's recorded path
// (JS blockParams[d].path[i]; "undefined" when the frame has none) plus
// "." + the child path; "true" for a subexpression; "null" for a literal.
func (s *renderState) paramID(n node) (string, error) {
	switch n := n.(type) {
	case *subExpr:
		return "true", nil
	case *pathExpr:
		if !n.bp {
			return n.legacy, nil
		}
		id := "undefined" // JS throws a TypeError reading the missing path; we stay lenient like the value lookup
		if i := len(s.bps) - 1 - n.bpDepth; i >= 0 && n.bpIdx < len(s.bps[i].ids) {
			id = s.bps[i].ids[n.bpIdx]
		}
		if n.legacy == "" {
			return id, nil
		}
		id = strings.Join([]string{id, n.legacy}, ".")
		return id, chargePath(s, id)
	}
	return "null", nil
}

// chargePath charges an op per KiB of a trackIds path (a contextPath or
// id) the render has just joined or copied: ids can be as long as the
// template and paths grow with nesting. Every join or copy of one is
// charged here: appendContextPath, paramID, and each's start and items.
// Storing or passing one on is free.
func chargePath[S string | []byte](s *renderState, p S) error { return s.steps(len(p) >> 10) }

// appendContextPath is utils.js appendContextPath(data.contextPath, id)
// for the current frame.
func (s *renderState) appendContextPath(id string) (string, error) {
	cp, _ := s.data.get("contextPath")
	if !jsTruthy(cp) {
		return id, nil
	}
	path := strings.Join([]string{jsKey(cp), id}, ".")
	return path, chargePath(s, path)
}

// helperThis is the JS helper call context: depth0 != null ? depth0 : nullContext.
func helperThis(ctx any) any {
	if isNull(ctx) {
		return nullCtx{}
	}
	return ctx
}

// helperMissing is the JS helperMissing hook: a missing {{foo}} is
// undefined; calling a missing helper with arguments is an error.
func helperMissing(o *HelperOptions) (any, error) {
	if len(o.Args) == 0 {
		return nil, nil
	}
	return nil, &Error{Message: "Missing helper: \"" + o.Name + "\""}
}

// blockHelperMissing is the JS blockHelperMissing hook: o.Args[0] is the
// block's value, o.Ctx() the enclosing context. It renders straight into
// the current output and returns nil.
func blockHelperMissing(o *HelperOptions) (any, error) {
	s, v := o.s, o.Args[0]
	switch {
	case v == true:
		return nil, s.renderProgram(o.fn, o.this)
	case v == false || isNull(v):
		return nil, s.renderProgram(o.inverse, o.this)
	}
	// JS options.ids is [] here (truthy) whenever trackIds is on.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		if rv.Len() == 0 {
			return nil, s.renderProgram(o.inverse, o.this)
		}
		if s.opts.TrackIDs {
			o.Legacy = &LegacyParams{IDs: []string{o.Name}}
		}
		h, builtin := s.helper("each") // JS instance.helpers.each, so an override applies
		return s.callHelper("each", h, builtin, o)
	}
	if s.opts.TrackIDs && s.data != nil {
		path, err := s.appendContextPath(o.Name)
		if err != nil {
			return nil, err
		}
		return nil, s.renderFrame(o.fn, v, bpFrame{}, &dataFrame{parent: s.data, contextPath: path})
	}
	return nil, s.renderProgram(o.fn, v)
}
