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
		o.buf = appendValue(o.buf, v, escape, mark+o.max-*o.emitted)
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
	out    *outWriter // current sink; renderSub swaps in a sub-writer
	spare  []byte     // a finished sub-writer's buffer, reused by the next one
	depths []any      // JS depths, innermost last: JS depths[n] is depths[len-1-n]
	depth  int        // nested block programs (partials will count too)
	// bps is the runtime.js blockParams chain: one frame per program being
	// rendered, innermost last (nil when the caller passed none). A path with
	// pathExpr.bp set reads bps[len-1-bpDepth][bpIdx].
	bps [][]any
	ops int
	// emitted is the output byte count every outWriter of this render shares.
	emitted int
	limits  Limits
	ctx     context.Context // never nil
	done    <-chan struct{} // ctx.Done(); nil when ctx can't be cancelled
}

func (t *Template) render(ctx context.Context, out *outWriter, data any) error {
	s := &renderState{t: t, out: out, limits: t.opts.Limits.withDefaults(), ctx: ctx, done: ctx.Done()}
	out.emitted, out.max = &s.emitted, s.limits.MaxOutputSize
	if err := ctx.Err(); err != nil {
		return err
	}
	s.depths = append(make([]any, 0, 8), data)
	if err := s.body(t.prog.body, data); err != nil {
		return err
	}
	// Both counters stay over once exceeded, so a LimitError a helper
	// swallowed still fails the render.
	if s.emitted > s.limits.MaxOutputSize {
		return &LimitError{What: "output size", Max: s.limits.MaxOutputSize}
	}
	if s.ops > s.limits.MaxOps {
		return &LimitError{What: "ops", Max: s.limits.MaxOps}
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
	if s.done != nil && s.ops%1024 == 0 {
		return s.ctx.Err()
	}
	return nil
}

// renderProgram renders a block program with ctx as `this`, like a
// runtime.js wrapProgram closure: ctx becomes the new depths[0] unless it
// equals the current one. Nesting is bounded by MaxRenderDepth.
func (s *renderState) renderProgram(p *program, ctx any) error {
	return s.renderProgramBP(p, ctx, nil)
}

// renderProgramBP is renderProgram with the block params the caller passes
// to the program (JS options.fn(ctx, {blockParams})).
func (s *renderState) renderProgramBP(p *program, ctx any, blockParams []any) error {
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
	s.bps = append(s.bps, blockParams)
	err := s.body(p.body, ctx)
	s.bps = s.bps[:len(s.bps)-1]
	s.depth--
	if push {
		s.depths = s.depths[:len(s.depths)-1]
	}
	return err
}

// renderString renders p with ctx to a string: JS options.fn(ctx).
func (s *renderState) renderString(p *program, ctx any) (string, error) {
	if p == nil {
		return "", nil
	}
	buf, err := s.renderSub(nil, p, ctx, nil)
	str := ""
	if err == nil {
		str = string(buf)
	}
	s.reuse(buf)
	return str, err
}

// renderTo renders p with ctx and block params into w.
func (s *renderState) renderTo(w io.Writer, p *program, ctx any, blockParams []any) error {
	if p == nil {
		return nil
	}
	buf, err := s.renderSub(w, p, ctx, blockParams)
	s.reuse(buf)
	return err
}

// renderSub renders p for a helper through a sub-writer that shares the
// render's byte budget and flushes to w (nil: everything stays in the
// returned buffer). Each call is one op, so a helper looping over an empty
// block is still bounded by MaxOps.
func (s *renderState) renderSub(w io.Writer, p *program, ctx any, blockParams []any) ([]byte, error) {
	if err := s.step(); err != nil {
		return nil, err
	}
	parent := s.out
	sub := &outWriter{w: w, buf: s.spare[:0], emitted: parent.emitted, max: parent.max}
	s.spare = nil
	s.out = sub
	err := s.renderProgramBP(p, ctx, blockParams)
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
		case *mustacheNode:
			if n.decorator {
				return errAt(n, "decorators are not supported yet")
			}
			var v any
			if v, err = s.expr(&n.call, ctx, false, nil, nil); err == nil {
				err = s.out.writeValue(v, n.escaped && !s.t.opts.NoEscape)
			}
		case *blockNode:
			if n.decorator {
				return errAt(n, "decorators are not supported yet")
			}
			var v any
			if v, err = s.expr(&n.call, ctx, true, n.program, n.inverse); err == nil {
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

// param evaluates a helper argument; paths are looked up, never called.
func (s *renderState) param(n node, ctx any) (any, error) {
	switch n := n.(type) {
	case *pathExpr:
		return s.lookup(n, ctx), nil
	case *subExpr:
		return s.expr(&n.call, ctx, false, nil, nil)
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
	v, parts := ctx, p.parts
	switch {
	case p.bp: // lookupBlockParam
		v = nil
		if i := len(s.bps) - 1 - p.bpDepth; i >= 0 && p.bpIdx < len(s.bps[i]) {
			v = s.bps[i][p.bpIdx]
		}
		parts = parts[1:]
	case p.depth > 0:
		i := len(s.depths) - 1 - p.depth
		if i < 0 {
			return nil
		}
		v = s.depths[i]
	case s.t.opts.Compat && !p.scoped && len(parts) > 0: // depthedLookup of the first part
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
