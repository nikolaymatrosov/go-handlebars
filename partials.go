package handlebars

import "strconv"

// Partials port compiler.js PartialStatement, runtime.js
// invokePartialWrapper / resolvePartial / invokePartial, and decorators
// (wrapProgram's executeDecorators, decorators/inline.js).

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
// own parse-time Options (Strict, NoEscape, ...), but its Limits and Data
// are ignored: those, like helpers and partials, come from the template
// being rendered. A nil p unregisters name. Registration is not
// concurrency-safe; register everything before rendering.
func (t *Template) RegisterPartialTemplate(name string, p *Template) {
	t.setPartial(name, partialDef{t: p}, p == nil)
}

// RegisterPartialFunc registers fn as partial name, like a handlebars.js
// function partial: fn gets the partial's context and its result is
// written unescaped (and indented when the partial is standalone). A nil
// fn unregisters name. Unlike JS, fn gets no options argument.
// Registration is not concurrency-safe; register everything before rendering.
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
	bps    []bpFrame
	opts   *Options
}

// closure binds p to depths and the current block params. Both are
// copied: the stack slots and {{#each}}'s block-param slice are reused once
// the creator moves on, and a saved @partial-block can be replayed later
// (FnTo dataVars). Copies have no spare capacity, so every run of the
// closure appends to its own, but for one spare bps slot: it takes the
// empty frame runClosure pushes, the same bpFrame{} from every run (nested
// ones too), so a run whose body nests no block copies nothing. ownFrame
// adds p's (param-less) frame. The copies share one []any and one
// []string, so a closure costs at most four allocations however deep the
// scope. The copy is O(depth), so it costs an op per 8 entries copied:
// deep scopes can't make each partial call do a hundred entries' work
// for one op.
func (s *renderState) closure(p *program, depths []any, ownFrame bool) (closure, error) {
	nv, ni := len(depths), 0
	for _, f := range s.bps {
		nv, ni = nv+len(f.vals), ni+len(f.ids)
	}
	n := len(s.bps)
	if ownFrame {
		n++
	}
	if err := s.steps((nv + ni + n) >> 3); err != nil {
		return closure{}, err
	}
	vals := append(make([]any, 0, nv), depths...) // depths, then each frame's vals
	ids := make([]string, 0, ni)
	bps := make([]bpFrame, n, n+1)
	for i, f := range s.bps {
		v, d := len(vals), len(ids)
		vals, ids = append(vals, f.vals...), append(ids, f.ids...)
		bps[i] = bpFrame{vals[v:len(vals):len(vals)], ids[d:len(ids):len(ids)]}
	}
	return closure{p, vals[:len(depths):len(depths)], bps, s.opts}, nil
}

// runClosure renders c's program with ctx in c's scope. c's stacks have no
// spare capacity past the bps slot its own frame takes, so a push of the
// context or of a nested block's frame copies them: an op per 8 entries,
// as closure charges.
func (s *renderState) runClosure(c *closure, ctx any) error {
	if err := s.steps((len(c.depths) + len(c.bps)) >> 3); err != nil {
		return err
	}
	s.depths, s.bps, s.opts = c.depths, c.bps, c.opts
	return s.renderProgramBP(c.prog, ctx, bpFrame{})
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
	if err == nil {
		err = s.steps(len(name) >> 10) // hashed by resolve
	}
	if err != nil {
		return err
	}
	pctx, err := s.partialContext(n, ctx)
	if err != nil {
		return err
	}
	prog, err := s.wrap(n.program) // JS options.fn: the block's decorators run once, here
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
	if prog != nil {
		c, err := s.closure(prog, s.depths, false)
		if err != nil {
			return err
		}
		block = &partialBlock{closure: c, inlines: s.inlines, outer: s.partialBlock()}
		if !tg.found() {
			tg.block = block // the failover content
		}
	}
	if s.opts.TrackIDs && s.data != nil { // JS throws a TypeError without data; we skip
		// runtime.js invokePartial: options.data.contextPath = options.ids[0]
		// || options.data.contextPath, on the caller's frame, as JS does.
		// ponytail: ids are strings, so a path named [null]/[undefined]
		// reads as falsy here; keep the JS id kinds if that ever matters.
		id := "" // the implicit context's
		switch {
		case n.hash != nil: // invokePartialWrapper: options.ids[0] = true
			id = "true"
		case len(n.params) > 0:
			if id, err = s.paramID(n.params[0]); err != nil {
				return err
			}
		}
		if id != "" && id != "null" && id != "undefined" {
			s.data.contextPath = id
		}
	}
	if !tg.found() {
		return &Error{Message: "The partial " + name + " could not be found"}
	}
	if s.depth >= s.limits.MaxRenderDepth {
		return s.depthErr()
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
		if len(block.prog.inlines) > 0 { // JS fn.partials: the block's inline partials reach the partial
			var c closure
			if c, err = s.closure(nil, s.depths, true); err == nil {
				err = s.addInlines(block.prog, c)
			}
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
		s.bps, s.opts = s.bps[len(s.bps):], &tg.def.t.opts
		p, err := s.wrap(tg.def.t.prog) // before ctx is pushed: JS main's decorators get options.depths || []
		if err != nil {
			return err
		}
		outer := s.depths
		if len(s.depths) == 0 || !sameContext(ctx, s.depths[len(s.depths)-1]) {
			s.depths = append(s.depths, ctx)
		}
		return s.run(p, ctx, outer)
	case tg.inline != nil:
		return s.runClosure(tg.inline, ctx)
	}
	b := tg.block // partialBlockWrapper: the block runs with the @partial-block it was written under
	s.inlines = b.inlines
	if s.data != nil {
		s.data = &dataFrame{parent: s.data, blockSet: true, block: b.outer}
	}
	return s.runClosure(&b.closure, ctx)
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
	} else if s.opts.StringParams { // the implicit context param, {parts: []}, as a string param
		pctx = ""
	}
	if n.hash == nil {
		return pctx, nil
	}
	obj := &object{vals: make(map[string]any, len(n.hash))}
	if !isNull(pctx) {
		i := 0
		err := forOwn(pctx, func(v any, key string, _ int, arr, _ bool) error {
			if arr {
				key = strconv.Itoa(i)
				i++
			}
			obj.set(key, v)
			return s.step() // a copied key is an op: a big context can't make each call O(len)
		})
		if err != nil {
			return nil, err
		}
	}
	for _, kv := range n.hash {
		v, err := s.param(kv.value, ctx)
		if err == nil {
			err = s.steps(len(kv.key) >> 10)
		}
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
		n := 1
		if len(e.name) == len(name) { // compared byte by byte
			n += len(name) >> 10
		}
		if err := s.steps(n); err != nil {
			return tg, err
		}
		if e.name == name {
			if e.prog != nil { // {{*inline "x"}} (no block) defines x as undefined
				tg.inline = &e.closure
			}
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

// run renders p's body (JS prog): the inline partials p's decorators
// defined are visible until p returns, as decorators/inline.js swaps
// container.partials around the program. outer is the depths before p's
// context was pushed; the inline partials run with them.
func (s *renderState) run(p *program, ctx any, outer []any) error {
	if p == nil { // a zero Template
		return nil
	}
	if len(p.inlines) == 0 {
		return s.body(p.body, ctx)
	}
	scope := s.inlines
	c, err := s.closure(nil, outer, false)
	if err == nil {
		err = s.addInlines(p, c)
	}
	if err == nil {
		err = s.body(p.body, ctx)
	}
	s.inlines = scope
	return err
}

// addInlines brings p's inline partials into scope, bound to sc: one op
// each, so a program full of them can't make every run expensive.
func (s *renderState) addInlines(p *program, sc closure) error {
	for _, ip := range p.inlines {
		if err := s.step(); err != nil {
			return err
		}
		sc.prog = ip.prog
		s.inlines = &inlinePartial{ip.name, sc, s.inlines}
	}
	return nil
}

// decorator is a registered decorator, JS (fn, props, container, options)
// => fn. props are the properties of the program it decorates: JS fn.x and
// props.x alike, which a block helper reads as options.fn.x. o holds the
// decorator's args, hash and own block (o.Fn() is JS options.fn()).
// ponytail: a decorator can't replace the program (no corpus test returns
// a new fn); add a wrapper return value if one ever must.
type decorator func(props map[string]any, o *HelperOptions) error

// wrap is runtime.js wrapProgram's executeDecorators. It runs p's
// decorators where JS hands a program out: once per block statement,
// partial block call and template render, never per run (an {{#each}}
// iteration reuses the wrap). s.depths must be the wrap-time depths
// (a template wraps before pushing its context); decorator params see
// their innermost entry (JS depth0: undefined for a template's own
// decorators). p is returned as is when it has none.
func (s *renderState) wrap(p *program) (*program, error) {
	if !p.decorated() {
		return p, nil
	}
	return s.decorate(p)
}

// decorated reports that p (maybe nil) has decorators still to run.
func (p *program) decorated() bool { return p != nil && p.decorators != nil }

// decorate runs p's decorators, one op each, and returns the copy of p
// holding their results (decorators already cleared: rewrapping it is a
// no-op). A user decorator runs under s.call's recover; inline is builtin.
func (s *renderState) decorate(p *program) (*program, error) {
	w := *p
	w.decorators = nil
	var ctx any
	if len(s.depths) > 0 {
		ctx = s.depths[len(s.depths)-1]
	}
	for _, d := range p.decorators {
		if err := s.step(); err != nil {
			return nil, err
		}
		var c *call
		var body *program
		switch d := d.(type) {
		case *mustacheNode:
			c = &d.call
		case *blockNode:
			c, body = &d.call, d.program
		default:
			return nil, errAt(d, "internal error: not a decorator")
		}
		name, _ := jsOriginal(c.path)
		dec := s.t.decorators[name]
		if dec == nil && name != "inline" {
			return nil, &Error{Message: "Missing decorator: \"" + name + "\""} // JS new Error: no loc
		}
		fn, err := s.wrap(body) // JS options.fn = container.program(...)
		if err != nil {
			return nil, err
		}
		o := &HelperOptions{Name: name, s: s, fn: fn} // JS calls decorators without a `this`
		if err := s.args(c, ctx, o); err != nil {
			return nil, err
		}
		// ponytail: inline shares the wrap path but stays a builtin branch,
		// not a registered decorator: its JS wrapper (swapping
		// container.partials) is run's scope binding, which a decorator
		// func can't express. Register it if decorators ever replace fn.
		if dec == nil { // decorators/inline.js: props.partials[args[0]] = options.fn
			var key any
			if len(o.Args) > 0 {
				key = o.Args[0]
			}
			w.inlines = append(w.inlines, inlinePartial{name: jsKey(key), closure: closure{prog: fn}})
			continue
		}
		// Only tests register decorators (spec_test.go): the spec harness needs this branch.
		if w.props == nil {
			w.props = map[string]any{}
		}
		if _, err := s.call(name, func(o *HelperOptions) (any, error) { return nil, dec(w.props, o) }, o); err != nil {
			return nil, err
		}
	}
	return &w, nil
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
