package handlebars

import (
	"slices"
	"strconv"
)

// Variables returns the paths into the root context that the template reads
// and that can be resolved statically, each as its segments, deduplicated, in
// order of first appearance. Call it after registering helpers: a bare name
// that matches a helper is a helper call, not a variable.
//
// Paths count where they render against the root context: the top level
// and, recursively, both branches of the builtin if and unless and the
// inverse ({{else}}, {{else if}} chains, {{^x}}) of the builtin each and
// with and of sections ({{#items}} on a variable), unless a helper of that
// name (for sections, a blockHelperMissing hook) is registered. Other
// block bodies, inline partials and partial blocks are skipped, except
// that @root.x is reported as x wherever it appears. A callee is a helper, not a
// variable, only when it is a bare name (one segment, no this, ./ or @) and
// the call has arguments or a hash, is a subexpression, or names a builtin,
// a KnownHelpers entry set to true or a helper registered on t. Literals,
// static partial names, other @data, block params, ../ paths, this, .,
// decorator names and arguments (but for @root.x) and the contents of
// registered partials are not reported: call Variables on those partials.
//
// Each inner slice is a fresh copy. Variables takes time linear in the
// template and is not bounded by Limits. It is safe to call concurrently
// with renders, but not while helpers or partials are being registered.
func (t *Template) Variables() [][]string {
	v := varWalker{t: t, seen: map[string]bool{}}
	v.program(t.prog, true)
	return v.out
}

// varWalker collects Template.Variables in one pass over the AST.
type varWalker struct {
	t    *Template
	seen map[string]bool // keys of the paths in out
	out  [][]string
	key  []byte // scratch for building seen keys
}

// program walks p's statements; root means they render against the root context.
func (v *varWalker) program(p *program, root bool) {
	if p == nil {
		return
	}
	for _, n := range p.body {
		switch n := n.(type) {
		case *mustacheNode: // a decorator's arguments only count for @root
			v.call(&n.call, root && !n.decorator, false)
		case *blockNode:
			v.call(&n.call, root && !n.decorator, false)
			var fn, inv bool // program and inverse render against the root context
			if root && !n.decorator {
				fn, inv = v.blockScope(&n.call)
			}
			if n.inverted { // the parser swapped them: the inverse comes first in the source
				v.program(n.inverse, inv)
				v.program(n.program, fn)
			} else {
				v.program(n.program, fn)
				v.program(n.inverse, inv)
			}
		case *partialNode:
			if s, ok := n.name.(*subExpr); ok {
				v.call(&s.call, root, true)
			}
			v.args(n.params, n.hash, root)
			v.program(n.program, false)
		}
	}
}

// call walks c's callee, unless it names a helper, then its arguments; sub
// means c is a subexpression.
func (v *varWalker) call(c *call, root, sub bool) {
	if p, ok := c.path.(*pathExpr); ok && !v.helperRef(p, c, sub) {
		v.path(p, root)
	}
	v.args(c.params, c.hash, root)
}

func (v *varWalker) args(params []node, hash []hashPair, root bool) {
	for _, n := range params {
		v.expr(n, root)
	}
	for _, h := range hash {
		v.expr(h.value, root)
	}
}

func (v *varWalker) expr(n node, root bool) {
	switch n := n.(type) {
	case *pathExpr:
		v.path(n, root)
	case *subExpr:
		v.call(&n.call, root, true)
	}
}

// helperRef reports whether callee p of c names a helper, not a variable.
func (v *varWalker) helperRef(p *pathExpr, c *call, sub bool) bool {
	if !bareName(p) {
		return false
	}
	name := p.parts[0]
	return sub || len(c.params) > 0 || c.hash != nil || isBuiltin(name) || v.t.opts.KnownHelpers[name] || v.registered(name)
}

// blockScope reports whether a block calling c renders its program and its
// inverse against the current context. The builtin if and unless render
// both there; the builtin each and with, and a section (a block on a
// variable, which the blockHelperMissing hook renders), only the inverse.
// A helper registered under that name, or as blockHelperMissing, could
// render either against anything.
func (v *varWalker) blockScope(c *call) (fn, inverse bool) {
	p, ok := c.path.(*pathExpr)
	if !ok {
		return false, false
	}
	if bareName(p) && !p.bp {
		switch name := p.parts[0]; {
		case v.registered(name):
			return false, false
		case name == "if" || name == "unless":
			return true, true
		case name == "each" || name == "with":
			return false, true
		}
	}
	section := len(c.params) == 0 && c.hash == nil && !v.helperRef(p, c, false)
	return false, section && !v.registered("blockHelperMissing")
}

func (v *varWalker) registered(name string) bool {
	_, ok := v.t.helpers[name]
	return ok
}

// bareName reports whether p is one plain segment: no @, ../, this or ./.
func bareName(p *pathExpr) bool {
	return !p.data && !p.scoped && p.depth == 0 && len(p.parts) == 1
}

// path records p if Variables reports it: @root.x anywhere, and in root
// scope a path into the context itself.
func (v *varWalker) path(p *pathExpr, root bool) {
	switch {
	case p.data:
		if p.depth == 0 && len(p.parts) > 1 && p.parts[0] == "root" {
			v.add(p.parts[1:])
		}
	case root && p.depth == 0 && !p.bp && len(p.parts) > 0:
		v.add(p.parts)
	}
}

// add appends a copy of parts to out unless an equal path is there already.
func (v *varWalker) add(parts []string) {
	v.key = v.key[:0]
	for _, s := range parts { // length-prefixed: a segment may hold any byte
		v.key = strconv.AppendInt(v.key, int64(len(s)), 10)
		v.key = append(v.key, ':')
		v.key = append(v.key, s...)
	}
	if !v.seen[string(v.key)] {
		v.seen[string(v.key)] = true
		v.out = append(v.out, slices.Clone(parts))
	}
}
