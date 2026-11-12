package handlebars

import (
	"fmt"
	"io"
	"reflect"
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
// only during the helper call.
type HelperOptions struct {
	Name string         // helper name as written in the template
	Args []any          // evaluated positional args
	Hash map[string]any // evaluated hash args; nil when the call has none

	s           *renderState
	this        any      // JS `this`
	fn, inverse *program // block programs; nil = none (container.noop)
}

// Ctx is the helper's `this`: the current context, or an empty object when
// that is null/undefined (JS container.nullContext).
func (o *HelperOptions) Ctx() any      { return o.this }
func (o *HelperOptions) IsBlock() bool { return o.fn != nil || o.inverse != nil }

// Fn renders the block with the helper's `this` (JS options.fn(this)).
func (o *HelperOptions) Fn() (string, error) { return o.FnWith(o.this) }

// FnWith renders the block with ctx (JS options.fn(ctx)).
func (o *HelperOptions) FnWith(ctx any) (string, error) { return o.s.renderString(o.fn, ctx) }

// FnTo renders the block with ctx and block params into w (JS
// options.fn(ctx, {blockParams})). The bytes count against MaxOutputSize;
// w is the helper's own sink, not the template's output.
func (o *HelperOptions) FnTo(w io.Writer, ctx any, blockParams []any, dataVars map[string]any) error {
	return o.s.renderTo(w, o.fn, ctx, blockParams) // Task 7: dataVars become the block's @data frame
}

// Inverse renders the {{else}} block with the helper's `this`.
func (o *HelperOptions) Inverse() (string, error) { return o.s.renderString(o.inverse, o.this) }

// InverseTo renders the {{else}} block with ctx into w (JS options.inverse(ctx)).
func (o *HelperOptions) InverseTo(w io.Writer, ctx any) error {
	return o.s.renderTo(w, o.inverse, ctx, nil)
}

// DataVar is the @name data variable visible to the helper.
func (o *HelperOptions) DataVar(name string) any { return nil } // Task 7: @data frames

// Lookup resolves a dot-separated property path ("a.b") from Ctx(), one JS
// options.lookupProperty per part. Function values are returned, not called.
// API change: "path" is a property chain only (no ../, this or [] segments).
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
func (s *renderState) helper(name string) Helper {
	if name == "helperMissing" || name == "blockHelperMissing" {
		return nil
	}
	return s.t.helpers[name] // Task 6: builtins
}

// hook is container.hooks[name]: the registered override or the builtin.
func (s *renderState) hook(name string, builtin Helper) Helper {
	if h := s.t.helpers[name]; h != nil {
		return h
	}
	return builtin
}

// call runs h. A panic in it becomes an error and the render state is
// restored, so the render fails cleanly even if the panic hit mid-block.
func (s *renderState) call(h Helper, o *HelperOptions) (v any, err error) {
	out, nd, nb, depth := s.out, len(s.depths), len(s.bps), s.depth
	defer func() {
		if r := recover(); r != nil {
			s.out, s.depths, s.bps, s.depth = out, s.depths[:nd], s.bps[:nb], depth
			v, err = nil, fmt.Errorf("handlebars: helper %q panicked: %v", o.Name, r)
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
		v := s.lookup(p, ctx)
		var err error
		if f, ok := asHelper(v); ok {
			v, err = s.call(f, &HelperOptions{s: s, this: ctx})
		}
		if err != nil || !block {
			return v, err
		}
		return s.call(s.hook("blockHelperMissing", blockHelperMissing),
			&HelperOptions{Name: p.original, Args: []any{v}, s: s, this: ctx, fn: fn, inverse: inverse})
	}
	// invokeAmbiguous: (helpers[name] || value), called if a function,
	// helperMissing if null.
	name := p.parts[0]
	h := s.helper(name)
	registered := h != nil
	var v any
	if !registered {
		v = s.lookup(p, ctx)
		if h, _ = asHelper(v); h == nil && isNull(v) {
			h = s.hook("helperMissing", helperMissing)
		}
	}
	var o *HelperOptions
	if h != nil {
		o = &HelperOptions{Name: name, s: s, this: helperThis(ctx), fn: fn, inverse: inverse}
		var err error
		if v, err = s.call(h, o); err != nil || registered {
			return v, err
		}
	}
	if !block {
		return v, nil
	}
	if o == nil {
		o = &HelperOptions{Name: name, s: s, fn: fn, inverse: inverse}
	}
	o.Args, o.this = []any{v}, ctx // ambiguousBlockValue reuses the options
	return s.call(s.hook("blockHelperMissing", blockHelperMissing), o)
}

// invokeHelper is JS helperSexpr: invokeKnownHelper,
// helpers[name].call(this, ...args, options), or invokeHelper,
// (helpers[name] || nonHelper || helperMissing).call(this, ...args, options).
func (s *renderState) invokeHelper(c *call, ctx any, fn, inverse *program) (any, error) {
	p := c.callee
	o := &HelperOptions{Name: p.original, Args: make([]any, len(c.params)), s: s, this: helperThis(ctx), fn: fn, inverse: inverse}
	for i, n := range c.params {
		v, err := s.param(n, ctx)
		if err != nil {
			return nil, err
		}
		o.Args[i] = v
	}
	if c.hash != nil {
		o.Hash = make(map[string]any, len(c.hash))
		for _, kv := range c.hash {
			v, err := s.param(kv.value, ctx)
			if err != nil {
				return nil, err
			}
			o.Hash[kv.key] = v
		}
	}
	if c.kind == kindKnown {
		o.Name = helperName(p)
		h := s.helper(o.Name)
		if h == nil { // JS: TypeError, helpers[name] is undefined
			return nil, &Error{Message: "Missing helper: \"" + o.Name + "\""}
		}
		return s.call(h, o)
	}
	var h Helper
	if len(p.parts) == 1 && !p.scoped && p.depth == 0 { // AST.helpers.simpleId
		h = s.helper(p.parts[0])
	}
	if h == nil {
		nonHelper := s.lookup(p, ctx)
		var ok bool
		if h, ok = asHelper(nonHelper); !ok {
			if jsTruthy(nonHelper) { // JS: TypeError, calling a non-function
				return nil, errAt(c.path, p.original+" is not a function")
			}
			h = s.hook("helperMissing", helperMissing)
		}
	}
	return s.call(h, o)
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
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		if rv.Len() == 0 {
			return nil, s.renderProgram(o.inverse, o.this)
		}
		// ponytail: JS calls helpers.each here; Task 6 swaps this loop for
		// it (adds @index/@first/@last and block params).
		for i := 0; i < rv.Len(); i++ {
			if err := s.step(); err != nil {
				return nil, err
			}
			if err := s.renderProgram(o.fn, rv.Index(i).Interface()); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	return nil, s.renderProgram(o.fn, v)
}
