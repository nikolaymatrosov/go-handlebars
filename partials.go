package handlebars

import (
	"slices"
	"strconv"
)

// Partials port compiler.js PartialStatement, runtime.js
// invokePartialWrapper / resolvePartial / invokePartial and the inline
// decorator (decorators/inline.js).

// partialDef is a registered partial: a parsed template or a function.
type partialDef struct {
	t  *Template
	fn func(ctx any) (string, error)
}

// RegisterPartial parses src with t's Options and registers it as name.
// Registration is not concurrency-safe; register everything before rendering.
func (t *Template) RegisterPartial(name, src string) error {
	p, err := ParseWithOptions(src, t.opts)
	if err != nil {
		return err
	}
	t.RegisterPartialTemplate(name, p)
	return nil
}

// RegisterPartialTemplate registers p as partial name. p renders with its
// own Options; helpers and partials come from the template being rendered.
// A nil p unregisters name.
func (t *Template) RegisterPartialTemplate(name string, p *Template) {
	t.setPartial(name, partialDef{t: p}, p == nil)
}

// RegisterPartialFunc registers fn as partial name, like a handlebars.js
// function partial: fn gets the partial's context and its result is
// written unescaped (and indented when the partial is standalone). A nil
// fn unregisters name.
// API change: new; the JS function's options argument is not passed.
func (t *Template) RegisterPartialFunc(name string, fn func(ctx any) (string, error)) {
	t.setPartial(name, partialDef{fn: fn}, fn == nil)
}

func (t *Template) setPartial(name string, d partialDef, remove bool) {
	if remove {
		delete(t.partials, name)
		return
	}
	if t.partials == nil {
		t.partials = map[string]partialDef{}
	}
	t.partials[name] = d
}

// closure is a block program with the scope it was written in (JS
// wrapProgram's depths and blockParams, plus its template's options):
// inline partials and partial blocks run in it wherever they are called.
type closure struct {
	prog   *program
	depths []any
	bps    [][]any
	opts   *Options
}

// closure binds p to depths and the current block params. Both are
// copied: the stack slots and {{#each}}'s block-param slice are reused once
// the creator moves on, and a saved @partial-block can be replayed later
// (FnTo dataVars). Copies have no spare capacity, so every run of the
// closure appends to its own. ownFrame adds p's (param-less) frame.
func (s *renderState) closure(p *program, depths []any, ownFrame bool) closure {
	n := len(s.bps)
	if ownFrame {
		n++
	}
	bps := make([][]any, n)
	for i, f := range s.bps {
		bps[i] = slices.Clone(f)
	}
	return closure{p, append(make([]any, 0, len(depths)), depths...), bps, s.opts}
}

// inlinePartial is a {{#*inline "name"}} block in scope (JS
// container.partials[name] set by the inline decorator). Scopes are
// immutable linked lists, innermost first, so capturing one is free.
type inlinePartial struct {
	name string
	closure
	next *inlinePartial
}

// partialBlock is the block of {{#> name}}block{{/name}}: JS
// data['partial-block'], which the partial renders with {{> @partial-block}}.
type partialBlock struct {
	closure
	inlines *inlinePartial // the caller's inline partials: they stay visible in the block
	outer   *partialBlock  // the @partial-block where the block was written
}

// partialBlock is the current @partial-block (nil if none).
func (s *renderState) partialBlock() *partialBlock {
	v, _ := s.data.get("partial-block")
	b, _ := v.(*partialBlock)
	return b
}

// target is a resolved partial; exactly one field is set.
type target struct {
	def    partialDef
	inline *closure
	block  *partialBlock
}

func (t target) found() bool {
	return t.def.t != nil || t.def.fn != nil || t.inline != nil || t.block != nil
}

// partial renders {{> name ctx k=v}} or {{#> name}}block{{/name}}. Each
// expansion is one op and one render-depth level, which bounds recursive
// partials.
func (s *renderState) partial(n *partialNode, ctx any) error {
	if err := s.step(); err != nil {
		return err
	}
	name, dynamic, err := s.partialName(n.name, ctx)
	if err != nil {
		return err
	}
	pctx, err := s.partialContext(n, ctx)
	if err != nil {
		return err
	}
	// resolvePartial sees the caller's partials, before the block's inline
	// partials join them.
	tg, err := s.resolve(name, dynamic)
	if err != nil {
		return err
	}
	var block *partialBlock
	if n.program != nil {
		block = &partialBlock{closure: s.closure(n.program, s.depths, false), inlines: s.inlines, outer: s.partialBlock()}
		if !tg.found() {
			tg.block = block // the failover content
		}
	}
	if !tg.found() {
		return &Error{Message: "The partial " + name + " could not be found"}
	}
	if s.depth >= s.limits.MaxRenderDepth {
		return &LimitError{What: "render depth", Max: s.limits.MaxRenderDepth}
	}
	out, indent := s.out, n.indent
	if indent != "" && s.opts.PreventIndent { // compiler.js: appendContent(indent)
		if err := out.writeString(indent); err != nil {
			return err
		}
		indent = ""
	}
	depths, bps, data, inlines, opts := s.depths, s.bps, s.data, s.inlines, s.opts
	s.depth++
	if indent != "" {
		out.pushIndent(indent)
	}
	if block != nil {
		if s.data != nil {
			s.data = &dataFrame{parent: s.data, blockSet: true, block: block}
		}
		if len(block.prog.decorators) > 0 { // JS fn.partials: the block's inline partials reach the partial
			err = s.decorate(block.prog, ctx, s.closure(nil, s.depths, true))
		}
	}
	if err == nil {
		err = s.invoke(tg, pctx, name)
	}
	if indent != "" {
		out.popIndent()
	}
	s.depth--
	s.depths, s.bps, s.data, s.inlines, s.opts = depths, bps, data, inlines, opts
	return err
}

// invoke renders the resolved partial with ctx; the caller restores the scope.
func (s *renderState) invoke(tg target, ctx any, name string) error {
	switch {
	case tg.def.fn != nil:
		var str string
		_, err := s.call(name, func(*HelperOptions) (any, error) {
			var err error
			str, err = tg.def.fn(ctx)
			return nil, err
		}, nil)
		if err != nil {
			return err
		}
		return s.out.writeString(str)
	case tg.def.t != nil: // a template's ret(context, {partial: true})
		if !s.opts.Compat { // only compat passes the caller's depths
			s.depths = s.depths[len(s.depths):]
		}
		outer := s.depths
		if len(s.depths) == 0 || !sameContext(ctx, s.depths[len(s.depths)-1]) {
			s.depths = append(s.depths, ctx)
		}
		s.bps, s.opts = s.bps[len(s.bps):], &tg.def.t.opts
		return s.run(tg.def.t.prog, ctx, outer)
	case tg.inline != nil:
		c := *tg.inline
		s.depths, s.bps, s.opts = c.depths, c.bps, c.opts
		return s.renderProgramBP(c.prog, ctx, nil)
	}
	b := tg.block // partialBlockWrapper: the block runs with the @partial-block it was written under
	s.depths, s.bps, s.opts, s.inlines = b.depths, b.bps, b.opts, b.inlines
	if s.data != nil {
		s.data = &dataFrame{parent: s.data, blockSet: true, block: b.outer}
	}
	return s.renderProgramBP(b.prog, ctx, nil)
}

// partialName is a static name's original, or the dynamic (subexpression)
// value as a JS property key.
func (s *renderState) partialName(n node, ctx any) (name string, dynamic bool, err error) {
	sub, ok := n.(*subExpr)
	if !ok {
		name, _ = jsOriginal(n)
		return name, false, nil
	}
	v, err := s.expr(&sub.call, ctx, false, nil, nil)
	if err != nil || !jsTruthy(v) { // resolvePartial: a falsy name looks up partials[undefined]
		return "undefined", true, err
	}
	return jsKey(v), true, nil
}

// partialContext is the partial's context: its param, else the current
// context (undefined with ExplicitPartialContext), extended with the hash
// (JS Utils.extend({}, context, hash)).
func (s *renderState) partialContext(n *partialNode, ctx any) (any, error) {
	pctx := ctx
	if len(n.params) > 0 {
		var err error
		if pctx, err = s.param(n.params[0], ctx); err != nil {
			return nil, err
		}
	} else if s.opts.ExplicitPartialContext {
		pctx = nil
	}
	if n.hash == nil {
		return pctx, nil
	}
	obj := &object{vals: make(map[string]any, len(n.hash))}
	if !isNull(pctx) {
		i := 0
		_ = forOwn(pctx, func(v any, key string, arr, _ bool) error {
			if arr {
				key = strconv.Itoa(i)
				i++
			}
			obj.set(key, v)
			return nil
		})
	}
	for _, kv := range n.hash {
		v, err := s.param(kv.value, ctx)
		if err != nil {
			return nil, err
		}
		obj.set(kv.key, v)
	}
	return obj, nil
}

// resolve finds partial name (JS resolvePartial): inline partials innermost
// first, then registered ones; a static @partial-block is the current block.
// Each inline partial scanned costs an op, so many of them can't make a
// partial call arbitrarily slow.
func (s *renderState) resolve(name string, dynamic bool) (tg target, err error) {
	for e := s.inlines; e != nil; e = e.next {
		if err := s.step(); err != nil {
			return tg, err
		}
		if e.name == name {
			tg.inline = &e.closure
			return tg, nil
		}
	}
	if d, ok := s.t.partials[name]; ok {
		tg.def = d
	} else if !dynamic && name == "@partial-block" {
		tg.block = s.partialBlock()
	}
	return tg, nil
}

// run renders p's body after its decorators (JS executeDecorators). The
// only decorator so far is inline; the partials it defines are visible
// until p returns, as decorators/inline.js swaps container.partials around
// the program. outer is the depths before p's context was pushed: JS hands
// decorators the wrap-time depths (options.depths || [] for a template).
func (s *renderState) run(p *program, ctx any, outer []any) error {
	if p == nil { // a zero Template
		return nil
	}
	if len(p.decorators) == 0 {
		return s.body(p.body, ctx)
	}
	scope := s.inlines
	err := s.decorate(p, ctx, s.closure(nil, outer, false))
	if err == nil {
		err = s.body(p.body, ctx)
	}
	s.inlines = scope
	return err
}

// decorate runs p's decorators, one op each: each {{#*inline "name"}}
// adds its block, in scope sc, to the inline partials.
func (s *renderState) decorate(p *program, ctx any, sc closure) error {
	for _, d := range p.decorators {
		if err := s.step(); err != nil {
			return err
		}
		b, ok := d.(*blockNode)
		if ok {
			path, _ := jsOriginal(b.path)
			ok = path == "inline"
		}
		if !ok {
			return errAt(d, "decorators are not supported yet") // Task 10b
		}
		var name any
		if len(b.params) > 0 {
			var err error
			if name, err = s.param(b.params[0], ctx); err != nil {
				return err
			}
		}
		sc.prog = b.program
		s.inlines = &inlinePartial{jsKey(name), sc, s.inlines}
	}
	return nil
}

// jsKey is JS String(v) used as a property name.
func jsKey(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "undefined"
	}
	return string(appendValue(nil, v, false, 1<<10))
}

// object is a JS object the renderer builds (a hash-extended partial
// context); keys keep insertion order.
type object struct {
	keys []string
	vals map[string]any
}

func (o *object) Keys() []string           { return o.keys }
func (o *object) Get(k string) (any, bool) { v, ok := o.vals[k]; return v, ok }
func (o *object) Len() int                 { return len(o.keys) }

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}
