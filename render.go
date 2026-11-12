package handlebars

import (
	"context"
	"io"
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

	// indents are the standalone partials being rendered into this
	// writer, outermost first; indents[bol:] are at a line start and owe
	// their indent before the next byte (JS invokePartialWrapper indents
	// every line of a partial's result but a trailing empty one).
	indents []string
	bol     int
	tmp     []byte // writeValue scratch while indenting
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
	if len(o.indents) > 0 {
		appendIndented(o, s, mark+o.max-*o.emitted)
	} else {
		o.buf = append(o.buf, s...)
	}
	return o.commit(mark)
}

// writeValue appends v like the JS `append` (escape=false) or
// `appendEscaped` (escapeExpression) opcodes. SafeString is never escaped.
func (o *outWriter) writeValue(v any, escape bool) error {
	if s, ok := v.(SafeString); ok {
		return o.writeString(string(s))
	}
	mark := len(o.buf)
	if len(o.indents) > 0 {
		o.tmp = appendValue(o.tmp[:0], v, escape, o.max-*o.emitted)
		appendIndented(o, o.tmp, mark+o.max-*o.emitted)
	} else {
		o.buf = appendValue(o.buf, v, escape, mark+o.max-*o.emitted)
	}
	return o.commit(mark)
}

// appendIndented appends s, writing the owed indents before the first
// byte of each line. It stops once buf is past limit (the caller's output
// budget), which commit then reports: indents repeated on every line could
// otherwise grow buf far beyond MaxOutputSize first.
func appendIndented[S string | []byte](o *outWriter, s S, limit int) {
	for len(s) > 0 {
		for _, in := range o.indents[o.bol:] {
			if len(o.buf) > limit {
				return
			}
			o.buf = append(o.buf, in...)
		}
		o.bol = len(o.indents)
		i := 0
		for i < len(s) && s[i] != '\n' {
			i++
		}
		if i < len(s) {
			i++
			o.bol = 0
		}
		o.buf = append(o.buf, s[:i]...)
		s = s[i:]
	}
}

func (o *outWriter) pushIndent(indent string) { o.indents = append(o.indents, indent) }

func (o *outWriter) popIndent() {
	o.indents = o.indents[:len(o.indents)-1]
	o.bol = min(o.bol, len(o.indents))
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
	t     *Template
	out   *outWriter // current sink; renderSub swaps in a sub-writer
	spare []byte     // a finished sub-writer's buffer, reused by the next one
	// depths are the JS depths, innermost last: JS depths[n] is
	// depths[len-1-n]. A partial without compat starts a fresh stack: a
	// view past the caller's entries, which appends never overwrite.
	depths []any
	depth  int // nested block programs and partial expansions
	// bps is the runtime.js blockParams chain: one frame per program being
	// rendered, innermost last (empty when the caller passed none). A path
	// with pathExpr.bp set reads bps[len-1-bpDepth].vals[bpIdx].
	bps []bpFrame
	// data is the current @data frame (JS options.data); nil when
	// Options.Data is false.
	data *dataFrame
	// inlines are the {{#*inline}} partials in scope, innermost first.
	inlines *inlinePartial
	opts    *Options // of the template being run: the root's or a partial's
	ops     int
	// depthHit: MaxRenderDepth was exceeded; the render fails even if a
	// helper dropped the error, as the ops and output counters stay over.
	depthHit bool
	// emitted is the output byte count every outWriter of this render shares.
	emitted int
	limits  Limits
	ctx     context.Context // never nil
	done    <-chan struct{} // ctx.Done(); nil when ctx can't be cancelled
}

// rootFrame is runtime.js initData: dataVars is used as is when it sets
// root; otherwise a child frame (createFrame) holds root = the context, so
// `@../x` from the root frame still reaches dataVars.
func rootFrame(ctx any, dataVars map[string]any) *dataFrame {
	if _, ok := dataVars["root"]; ok {
		return &dataFrame{vars: dataVars}
	}
	f := &dataFrame{vars: map[string]any{"root": ctx}}
	if dataVars != nil {
		f.parent = &dataFrame{vars: dataVars}
	}
	return f
}

func (t *Template) render(ctx context.Context, out *outWriter, data any, dataVars map[string]any) error {
	s := &renderState{t: t, out: out, opts: &t.opts, limits: t.opts.Limits.withDefaults(), ctx: ctx, done: ctx.Done()}
	out.emitted, out.max = &s.emitted, s.limits.MaxOutputSize
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.opts.Data == nil || *t.opts.Data {
		s.data = rootFrame(data, dataVars)
	}
	p, err := s.wrap(t.prog) // before depths are seeded: JS main's decorators get options.depths || []
	if err == nil {
		s.depths = append(make([]any, 0, 8), data)
		err = s.run(p, data, nil)
	}
	if err != nil {
		return err
	}
	// Limits stay hit once exceeded, so a LimitError a helper swallowed
	// still fails the render.
	if s.depthHit {
		return &LimitError{What: "render depth", Max: s.limits.MaxRenderDepth}
	}
	if s.emitted > s.limits.MaxOutputSize {
		return &LimitError{What: "output size", Max: s.limits.MaxOutputSize}
	}
	if s.ops > s.limits.MaxOps {
		return &LimitError{What: "ops", Max: s.limits.MaxOps}
	}
	return out.flush()
}

// step counts one op and, every 1024 ops, checks the RenderCtx context.
// Ops are substitutions, helper calls and loop iterations, plus whatever a
// statement does in proportion to its size or its data (arguments, path
// segments, comments, partial expansions, decorators, copied keys), so
// the work per op is bounded.
func (s *renderState) step() error {
	s.ops++
	if s.ops > s.limits.MaxOps {
		return &LimitError{What: "ops", Max: s.limits.MaxOps}
	}
	if s.done != nil && s.ops%1024 == 0 {
		return s.ctx.Err()
	}
	return nil
}

// steps charges n ops. Besides one per item of work, a render charges
// one per KiB of a long template string it hashes, compares or copies
// (n = len >> 10), so CPU per op stays bounded however long names are.
func (s *renderState) steps(n int) error {
	for range n {
		if err := s.step(); err != nil {
			return err
		}
	}
	return nil
}

// depthErr records and returns the MaxRenderDepth LimitError.
func (s *renderState) depthErr() error {
	s.depthHit = true
	return &LimitError{What: "render depth", Max: s.limits.MaxRenderDepth}
}

// renderProgram renders a block program with ctx as `this`, like a
// runtime.js wrapProgram closure: ctx becomes the new depths[0] unless it
// equals the current one. Nesting is bounded by MaxRenderDepth.
func (s *renderState) renderProgram(p *program, ctx any) error {
	return s.renderProgramBP(p, ctx, bpFrame{})
}

// bpFrame is the block params a caller passes to a program (JS
// options.fn(ctx, {blockParams})); ids is their JS blockParams.path, which
// TrackIDs reads. nil ids: no path (FnTo).
type bpFrame struct {
	vals []any
	ids  []string
}

// renderProgramBP is renderProgram with the block params the caller passes
// to the program.
func (s *renderState) renderProgramBP(p *program, ctx any, blockParams bpFrame) error {
	if p == nil {
		return nil // container.noop
	}
	if s.depth >= s.limits.MaxRenderDepth {
		return s.depthErr()
	}
	outer := s.depths
	var top any // JS depths[0]; undefined when an inline partial captured []
	if len(outer) > 0 {
		top = outer[len(outer)-1]
	}
	push := !sameContext(ctx, top) && !(ctx == any(nullCtx{}) && len(outer) > 0 && isNull(top))
	if push {
		s.depths = append(s.depths, ctx)
	}
	s.depth++
	s.bps = append(s.bps, blockParams)
	err := s.run(p, ctx, outer)
	s.bps = s.bps[:len(s.bps)-1]
	s.depth--
	if push {
		s.depths = s.depths[:len(s.depths)-1]
	}
	return err
}

// renderFrame is renderProgramBP with df as the block's @data frame (JS
// options.data); a nil df keeps the enclosing one.
func (s *renderState) renderFrame(p *program, ctx any, blockParams bpFrame, df *dataFrame) error {
	if df == nil {
		return s.renderProgramBP(p, ctx, blockParams)
	}
	prev := s.data
	s.data = df
	err := s.renderProgramBP(p, ctx, blockParams)
	s.data = prev
	return err
}

// renderString renders p with ctx to a string: JS options.fn(ctx).
func (s *renderState) renderString(p *program, ctx any) (string, error) {
	if p == nil {
		return "", nil
	}
	buf, err := s.renderSub(nil, p, ctx, bpFrame{}, nil)
	str := ""
	if err == nil {
		str = string(buf)
	}
	s.reuse(buf)
	return str, err
}

// renderTo renders p with ctx and block params into w.
func (s *renderState) renderTo(w io.Writer, p *program, ctx any, blockParams bpFrame, df *dataFrame) error {
	if p == nil {
		return nil
	}
	buf, err := s.renderSub(w, p, ctx, blockParams, df)
	s.reuse(buf)
	return err
}

// renderSub renders p for a helper through a sub-writer that shares the
// render's byte budget and flushes to w (nil: everything stays in the
// returned buffer). Each call is one op, so a helper looping over an empty
// block is still bounded by MaxOps.
func (s *renderState) renderSub(w io.Writer, p *program, ctx any, blockParams bpFrame, df *dataFrame) ([]byte, error) {
	if err := s.step(); err != nil {
		return nil, err
	}
	parent := s.out
	sub := &outWriter{w: w, buf: s.spare[:0], emitted: parent.emitted, max: parent.max}
	s.spare = nil
	s.out = sub
	err := s.renderFrame(p, ctx, blockParams, df)
	s.out = parent
	if err == nil {
		err = sub.flush()
	}
	return sub.buf, err
}

// reuse keeps the larger of buf and the current spare for the next sub-writer.
func (s *renderState) reuse(buf []byte) {
	if cap(buf) > cap(s.spare) {
		s.spare = buf
	}
}

func (s *renderState) body(nodes []node, ctx any) error {
	for _, n := range nodes {
		var err error
		switch n := n.(type) {
		case *contentNode:
			err = s.out.writeString(n.value)
		case *commentNode:
			err = s.step()
		case *mustacheNode:
			if n.decorator {
				continue // run when the program is wrapped
			}
			var v any
			if v, err = s.expr(&n.call, ctx, false, nil, nil); err == nil {
				err = s.out.writeValue(v, n.escaped && !s.opts.NoEscape)
			}
		case *blockNode:
			if n.decorator {
				continue // run when the program is wrapped
			}
			fn, inverse := n.program, n.inverse
			if fn.decorated() || inverse.decorated() {
				if fn, inverse, err = s.wrapBlock(n); err != nil {
					return err
				}
			}
			var v any
			if v, err = s.expr(&n.call, ctx, true, fn, inverse); err == nil {
				err = s.out.writeValue(v, false)
			}
		case *partialNode:
			err = s.partial(n, ctx)
		default:
			return errAt(n, "unknown node")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// wrapBlock wraps a block statement's programs (JS options.fn/inverse =
// container.program(...)), running their decorators before the helper
// sees them.
// ponytail: JS wraps after evaluating the callee and params, so user code
// there (param/subexpression helpers, data lambdas, helperMissing) runs
// after the decorators instead of before, and decorators still run (and
// cost ops) when the callee or params throw. Only inline-name
// subexpressions can observe it today; wrap inside expr/invokeHelper if needed.
func (s *renderState) wrapBlock(n *blockNode) (fn, inverse *program, err error) {
	if fn, err = s.wrap(n.program); err == nil {
		inverse, err = s.wrap(n.inverse)
	}
	return fn, inverse, err
}

// errAt is a runtime *Error positioned at n.
func errAt(n node, msg string) error {
	line, col := n.pos()
	return &Error{Line: line, Column: col, Message: msg}
}

// param evaluates a helper argument; paths are looked up, never called.
// Under StringParams (compiler.js pushParam) a path is its string form and
// undefined/null are "".
func (s *renderState) param(n node, ctx any) (any, error) {
	if err := s.step(); err != nil {
		return nil, err
	}
	switch n := n.(type) {
	case *pathExpr:
		if s.opts.StringParams {
			return n.legacy, nil
		}
		return s.lookup(n, ctx, false)
	case *subExpr:
		return s.expr(&n.call, ctx, false, nil, nil)
	case *strLit:
		if err := s.steps(len(n.value) >> 10); err != nil { // a key the callee hashes
			return nil, err
		}
		if s.opts.StringParams {
			return n.legacy, nil
		}
		return n.value, nil
	case *numLit:
		return n.value, nil
	case *boolLit:
		return n.value, nil
	}
	if s.opts.StringParams { // val.original || ''
		return "", nil
	}
	return nil, nil // undefined, null
}

// lookup resolves a path like lookupOnContext: depth picks the context,
// then each part is a lookupProperty, stopping at null/undefined.
// site reports a strict use site (JS path.strict): the callee of a mustache,
// block or helper call, whose last part must exist in strict mode.
func (s *renderState) lookup(p *pathExpr, ctx any, site bool) (any, error) {
	n := len(p.parts) - 1 // each further segment is a property read
	for _, part := range p.parts {
		n += len(part) >> 10 // hashed by the read
	}
	if err := s.steps(n); err != nil {
		return nil, err
	}
	if s.opts.Strict || s.opts.AssumeObjects {
		return s.lookupStrict(p, ctx, s.opts.Strict && site)
	}
	return s.lookupLoose(p, ctx), nil
}

func (s *renderState) lookupLoose(p *pathExpr, ctx any) any {
	v, parts := ctx, p.parts
	switch {
	case p.data: // lookupData: depth walks _parent, then the first part is a frame var
		f := s.data
		for d := p.depth; d > 0 && f != nil; d-- {
			f = f.parent
		}
		if f == nil || len(parts) == 0 {
			return nil
		}
		v, _ = f.get(parts[0])
		parts = parts[1:]
	case p.bp: // lookupBlockParam
		v = nil
		if i := len(s.bps) - 1 - p.bpDepth; i >= 0 && p.bpIdx < len(s.bps[i].vals) {
			v = s.bps[i].vals[p.bpIdx]
		}
		parts = parts[1:]
	case p.depth > 0:
		i := len(s.depths) - 1 - p.depth
		if i < 0 {
			return nil
		}
		v = s.depths[i]
	case s.opts.Compat && !p.scoped && len(parts) > 0: // depthedLookup of the first part
		v = s.depthedLookup(parts[0])
		parts = parts[1:]
	}
	for _, part := range parts {
		if v == nil {
			return nil
		}
		v, _ = lookupProperty(v, part)
	}
	return v
}

// depthedLookup is runtime.js container.lookup (compat mode): the first
// context, innermost out, that has a non-null property name.
func (s *renderState) depthedLookup(name string) any {
	for i := len(s.depths) - 1; i >= 0; i-- {
		if d := s.depths[i]; jsTruthy(d) {
			if v, _ := lookupProperty(d, name); !isNull(v) {
				return v
			}
		}
	}
	return nil
}

// lookupStrict is javascript-compiler.js strictLookup. Intermediate parts
// are dereferenced unchecked (a null one is a TypeError); with terminal the
// last part must exist (runtime.js container.strict).
func (s *renderState) lookupStrict(p *pathExpr, ctx any, terminal bool) (any, error) {
	v, parts, i := ctx, p.parts, 0
	var f *dataFrame
	switch {
	case p.data:
		f = s.data
		for d := p.depth; d > 0 && f != nil; d-- {
			f = f.parent
		}
	case p.bp:
		v = nil
		if j := len(s.bps) - 1 - p.bpDepth; j >= 0 && p.bpIdx < len(s.bps[j].vals) {
			v = s.bps[j].vals[p.bpIdx]
		}
		i, terminal = 1, false
	case p.depth > 0:
		j := len(s.depths) - 1 - p.depth
		if j < 0 {
			return nil, nil
		}
		v = s.depths[j]
	case s.opts.Compat && !p.scoped && len(parts) > 0:
		v, i = s.depthedLookup(parts[0]), 1
	}
	if len(parts) == 0 {
		return v, nil
	}
	n := len(parts)
	if terminal {
		n--
	}
	if p.data && i == 0 {
		switch {
		case terminal && n == 0:
			if f == nil {
				return nil, strictErr(p, nil, parts[0])
			}
			if w, ok := f.get(parts[0]); ok {
				return w, nil
			}
			return nil, strictErr(p, f, parts[0]) // JS passes the data object
		case f == nil:
			return nil, typeErr(parts[0])
		}
		v, _ = f.get(parts[0])
		i = 1
	}
	for ; i < n; i++ {
		if isNull(v) {
			return nil, typeErr(parts[i])
		}
		v, _ = lookupProperty(v, parts[i])
	}
	if terminal {
		w, ok := lookupProperty(v, parts[n])
		if !jsTruthy(v) || !ok {
			return nil, strictErr(p, v, parts[n])
		}
		v = w
	}
	return v, nil
}

// strictErr is the container.strict Exception, located at the path.
func strictErr(p *pathExpr, obj any, name string) error {
	str := "undefined"
	if obj != nil {
		str = string(appendValue(nil, obj, false, 1<<10))
	}
	return errAt(p, "\""+name+"\" not defined in "+str)
}

// typeErr stands for the JS TypeError of reading a property of null.
func typeErr(name string) error {
	return &Error{Message: "Cannot read properties of undefined (reading '" + name + "')"}
}
