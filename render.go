package handlebars

import (
	"context"
	"io"
	"reflect"
)

// The renderer walks the AST the way handlebars.js compiled code runs:
// classification and lookup follow compiler/compiler.js and
// javascript-compiler.js, the runtime hooks follow runtime.js and helpers/.

// flushSize is how much output RenderTo buffers before writing to w.
const flushSize = 4096

// outWriter is the single sink for rendered bytes. Every write counts
// against MaxOutputSize. Sub-writers (helper Fn() strings) share the count,
// so the limit bounds all bytes a render produces, not just the final output.
type outWriter struct {
	w       io.Writer // nil: keep everything in buf
	buf     []byte
	emitted *int // bytes produced by the whole render; shared with sub-writers
	max     int
}

// commit counts the bytes appended to buf since mark and flushes when due.
// Past the limit it drops them and fails; the count stays over, so every
// later write fails too.
func (o *outWriter) commit(mark int) error {
	*o.emitted += len(o.buf) - mark
	if *o.emitted > o.max {
		o.buf = o.buf[:mark]
		return &LimitError{What: "output size", Max: o.max}
	}
	if o.w != nil && len(o.buf) >= flushSize {
		return o.flush()
	}
	return nil
}

func (o *outWriter) writeString(s string) error {
	mark := len(o.buf)
	o.buf = append(o.buf, s...)
	return o.commit(mark)
}

// writeValue appends v like the JS `append` (escape=false) or
// `appendEscaped` (escapeExpression) opcodes. SafeString is never escaped.
func (o *outWriter) writeValue(v any, escape bool) error {
	mark := len(o.buf)
	if s, ok := v.(SafeString); ok {
		o.buf = append(o.buf, s...)
	} else {
		o.buf = appendValue(o.buf, v, escape)
	}
	return o.commit(mark)
}

func (o *outWriter) flush() error {
	if o.w == nil || len(o.buf) == 0 {
		return nil
	}
	_, err := o.w.Write(o.buf)
	o.buf = o.buf[:0]
	return err
}

// renderState is one render's mutable state.
type renderState struct {
	t      *Template
	out    *outWriter // current sink; renderString swaps in a sub-writer
	depths []any      // JS depths, innermost last: JS depths[n] is depths[len-1-n]
	depth  int        // nested block programs (partials will count too)
	ops    int
	// emitted is the output byte count every outWriter of this render shares.
	emitted int
	limits  Limits
	ctx     context.Context // nil outside RenderCtx
}

func (t *Template) render(ctx context.Context, out *outWriter, data any) error {
	s := &renderState{t: t, out: out, limits: t.opts.Limits.withDefaults(), ctx: ctx}
	out.emitted, out.max = &s.emitted, s.limits.MaxOutputSize
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	s.depths = append(make([]any, 0, 8), data)
	if err := s.body(t.prog.body, data); err != nil {
		return err
	}
	return out.flush()
}

// step counts one op (substitution, helper call or loop iteration) and,
// every 1024 ops, checks the RenderCtx context.
func (s *renderState) step() error {
	s.ops++
	if s.ops > s.limits.MaxOps {
		return &LimitError{What: "ops", Max: s.limits.MaxOps}
	}
	if s.ctx != nil && s.ops%1024 == 0 {
		return s.ctx.Err()
	}
	return nil
}

// renderProgram renders a block program with ctx as `this`, like a
// runtime.js wrapProgram closure: ctx becomes the new depths[0] unless it
// equals the current one. Nesting is bounded by MaxRenderDepth.
func (s *renderState) renderProgram(p *program, ctx any) error {
	if p == nil {
		return nil // container.noop
	}
	if s.depth >= s.limits.MaxRenderDepth {
		return &LimitError{What: "render depth", Max: s.limits.MaxRenderDepth}
	}
	top := s.depths[len(s.depths)-1]
	push := !sameContext(ctx, top) && !(ctx == any(nullCtx{}) && isNull(top))
	if push {
		s.depths = append(s.depths, ctx)
	}
	s.depth++
	err := s.body(p.body, ctx)
	s.depth--
	if push {
		s.depths = s.depths[:len(s.depths)-1]
	}
	return err
}

// renderString renders p with ctx into a fresh buffer: JS options.fn(ctx)
// returning a string.
func (s *renderState) renderString(p *program, ctx any) (string, error) {
	if p == nil {
		return "", nil
	}
	parent := s.out
	sub := &outWriter{emitted: parent.emitted, max: parent.max}
	s.out = sub
	err := s.renderProgram(p, ctx)
	s.out = parent
	if err != nil {
		return "", err
	}
	return string(sub.buf), nil
}

func (s *renderState) body(nodes []node, ctx any) error {
	for _, n := range nodes {
		var err error
		switch n := n.(type) {
		case *contentNode:
			err = s.out.writeString(n.value)
		case *commentNode:
		case *mustacheNode:
			if n.decorator {
				return errAt(n, "decorators are not supported yet")
			}
			var v any
			if v, err = s.expr(&n.call, ctx, false, false, nil, nil); err == nil {
				err = s.out.writeValue(v, n.escaped && !s.t.opts.NoEscape)
			}
		case *blockNode:
			if n.decorator {
				return errAt(n, "decorators are not supported yet")
			}
			var v any
			if v, err = s.expr(&n.call, ctx, false, true, n.program, n.inverse); err == nil {
				err = s.out.writeValue(v, false)
			}
		case *partialNode:
			return errAt(n, "partials are not supported yet")
		default:
			return errAt(n, "unknown node")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// errAt is a runtime *Error positioned at n.
func errAt(n node, msg string) error {
	line, col := n.pos()
	return &Error{Line: line, Column: col, Message: msg}
}

// expr evaluates a mustache, block or subexpression call. Like the JS
// compiler it turns a literal callee into a path (transformLiteralToPath),
// then classifySexpr picks: helper (params, hash, or a subexpression),
// ambiguous (a simple id: helper or context value) or simple (a scoped or
// multi-part path, resolved as a possible lambda). A block whose value is
// not a registered helper's result goes through blockHelperMissing
// (blockValue / ambiguousBlockValue).
func (s *renderState) expr(c *call, ctx any, sub, block bool, fn, inverse *program) (any, error) {
	if err := s.step(); err != nil {
		return nil, err
	}
	p := callPath(c.path)
	simpleID := len(p.parts) == 1 && !p.scoped && p.depth == 0
	if sub || len(c.params) > 0 || c.hash != nil {
		return s.invokeHelper(c, p, simpleID, ctx, fn, inverse)
	}
	if !simpleID { // resolvePossibleLambda: a function is called with `this` = ctx and no options
		v := s.lookup(p, ctx)
		var err error
		if f, ok := asHelper(v); ok {
			v, err = f(&HelperOptions{s: s, this: ctx})
		}
		if err != nil || !block {
			return v, err
		}
		return blockHelperMissing(&HelperOptions{Name: p.original, Args: []any{v}, s: s, this: ctx, fn: fn, inverse: inverse})
	}
	// invokeAmbiguous: (helpers[name] || value), called if a function,
	// helperMissing if null.
	name := p.parts[0]
	h := s.t.helpers[name]
	registered := h != nil
	var v any
	if !registered {
		v = s.lookup(p, ctx)
		if h, _ = asHelper(v); h == nil && isNull(v) {
			h = helperMissing
		}
	}
	var o *HelperOptions
	if h != nil {
		o = &HelperOptions{Name: name, s: s, this: helperThis(ctx), fn: fn, inverse: inverse}
		var err error
		if v, err = h(o); err != nil || registered {
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
	return blockHelperMissing(o)
}

// invokeHelper is JS helperSexpr + invokeHelper:
// (helpers[name] || nonHelper || helperMissing).call(this, ...args, options).
func (s *renderState) invokeHelper(c *call, p *pathExpr, simpleID bool, ctx any, fn, inverse *program) (any, error) {
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
	var h Helper
	if simpleID {
		h = s.t.helpers[p.parts[0]]
	}
	if h == nil {
		nonHelper := s.lookup(p, ctx)
		var ok bool
		if h, ok = asHelper(nonHelper); !ok {
			if jsTruthy(nonHelper) { // JS: TypeError, calling a non-function
				return nil, errAt(c.path, p.original+" is not a function")
			}
			h = helperMissing
		}
	}
	return h(o)
}

// param evaluates a helper argument; paths are looked up, never called.
func (s *renderState) param(n node, ctx any) (any, error) {
	switch n := n.(type) {
	case *pathExpr:
		return s.lookup(n, ctx), nil
	case *subExpr:
		return s.expr(&n.call, ctx, true, false, nil, nil)
	case *strLit:
		return n.value, nil
	case *numLit:
		return n.value, nil
	case *boolLit:
		return n.value, nil
	}
	return nil, nil // undefined, null
}

// lookup resolves a path like lookupOnContext: depth picks the context,
// then each part is a lookupProperty, stopping at null/undefined.
func (s *renderState) lookup(p *pathExpr, ctx any) any {
	if p.data {
		return nil // Task 7: @data
	}
	v := ctx
	if p.depth > 0 {
		i := len(s.depths) - 1 - p.depth
		if i < 0 {
			return nil
		}
		v = s.depths[i]
	}
	for _, part := range p.parts {
		if v == nil {
			return nil
		}
		v, _ = lookupProperty(v, part)
	}
	return v
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
