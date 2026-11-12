package handlebars

import (
	"strings"
	"unicode/utf8"
)

// wsControl ports compiler/whitespace-control.js: applies `~` strips and
// removes the whitespace around standalone blocks, comments and partials.
type wsControl struct {
	standalone bool // !Options.IgnoreStandalone
	rootSeen   bool
}

// wsStrip is what a statement reports to its parent program (JS `strip`).
type wsStrip struct {
	open, close               bool     // `~` towards the previous / next sibling
	openSA, closeSA, inlineSA bool     // standalone candidacy
	openProg, closeProg       *program // block: (program || inverse), (inverse || program)
}

func (w *wsControl) program(p *program) {
	isRoot := !w.rootSeen
	w.rootSeen = true
	body := p.body
	for i, cur := range body {
		s, ok := w.statement(cur)
		if !ok {
			continue
		}
		prevWS, nextWS := isPrevWhitespace(body, i, isRoot), isNextWhitespace(body, i, isRoot)
		if s.close {
			omitRight(body, i+1, true)
		}
		if s.open {
			omitLeft(body, i-1, true)
		}
		if !w.standalone {
			continue
		}
		if s.inlineSA && prevWS && nextWS {
			omitRight(body, i+1, false)
			if omitLeft(body, i-1, false) {
				pn, isPartial := cur.(*partialNode)
				if prev, ok := body[i-1].(*contentNode); ok && isPartial && pn.program == nil {
					pn.indent = prev.original[len(strings.TrimRight(prev.original, " \t")):]
				}
			}
		}
		if s.openSA && prevWS {
			omitRight(s.openProg.body, 0, false)
			omitLeft(body, i-1, false)
		}
		if s.closeSA && nextWS {
			omitRight(body, i+1, false)
			omitLeft(s.closeProg.body, len(s.closeProg.body)-1, false)
		}
	}
}

// statement visits n; ok is false for content (JS returns undefined).
func (w *wsControl) statement(n node) (wsStrip, bool) {
	switch n := n.(type) {
	case *mustacheNode:
		return wsStrip{open: n.strip.open, close: n.strip.close}, true
	case *commentNode:
		return wsStrip{open: n.strip.open, close: n.strip.close, inlineSA: true}, true
	case *partialNode:
		if n.program == nil {
			return wsStrip{open: n.strip.open, close: n.strip.close, inlineSA: true}, true
		}
		return w.block(n.program, nil, n.strip, stripFlags{}, n.closeStrip), true
	case *blockNode:
		return w.block(n.program, n.inverse, n.openStrip, n.inverseStrip, n.closeStrip), true
	}
	return wsStrip{}, false
}

// block is JS BlockStatement/DecoratorBlock/PartialBlockStatement.
func (w *wsControl) block(prog, inv *program, openStrip, inverseStrip, closeStrip stripFlags) wsStrip {
	if prog != nil {
		w.program(prog)
	}
	if inv != nil {
		w.program(inv)
	}
	// JS: program = block.program || block.inverse; inverse = block.program && block.inverse
	main, inverse, closeProg := prog, inv, inv
	if prog == nil {
		main, inverse = inv, nil
	}
	if closeProg == nil {
		closeProg = prog
	}
	firstInv, lastInv := inverse, inverse
	if inverse != nil && inverse.chained && len(inverse.body) == 1 {
		if b, ok := inverse.body[0].(*blockNode); ok {
			// JS walks lastInverse.body[last].program while chained, but a
			// block's program is never chained, so the walk stops here too.
			firstInv, lastInv = b.program, b.program
		}
	}
	closeOf := main
	if firstInv != nil {
		closeOf = firstInv
	}
	s := wsStrip{
		open:      openStrip.open,
		close:     closeStrip.close,
		openSA:    isNextWhitespace(main.body, -1, false),
		closeSA:   isPrevWhitespace(closeOf.body, len(closeOf.body), false),
		openProg:  main,
		closeProg: closeProg,
	}
	if openStrip.close {
		omitRight(main.body, 0, true)
	}
	if inverse != nil {
		if inverseStrip.open {
			omitLeft(main.body, len(main.body)-1, true)
		}
		if inverseStrip.close {
			omitRight(firstInv.body, 0, true)
		}
		if closeStrip.open {
			omitLeft(lastInv.body, len(lastInv.body)-1, true)
		}
		// standalone {{else}}
		if w.standalone && isPrevWhitespace(main.body, len(main.body), false) &&
			isNextWhitespace(firstInv.body, -1, false) {
			omitLeft(main.body, len(main.body)-1, false)
			omitRight(firstInv.body, 0, false)
		}
	} else if closeStrip.open {
		omitLeft(main.body, len(main.body)-1, true)
	}
	return s
}

// isPrevWhitespace: body[i-1] is content ending in a newline plus optional
// whitespace (or, at the root's first statement, only whitespace); with no
// previous node it is isRoot.
func isPrevWhitespace(body []node, i int, isRoot bool) bool {
	if i <= 0 {
		return isRoot
	}
	prev, ok := body[i-1].(*contentNode)
	if !ok {
		return false
	}
	t := trimRightSpace(prev.original)
	return strings.Contains(prev.original[len(t):], "\n") || (i < 2 && isRoot && t == "")
}

// isNextWhitespace mirrors isPrevWhitespace for body[i+1].
func isNextWhitespace(body []node, i int, isRoot bool) bool {
	if i+1 >= len(body) {
		return isRoot
	}
	next, ok := body[i+1].(*contentNode)
	if !ok {
		return false
	}
	j := skipSpace(next.original, 0)
	return strings.Contains(next.original[:j], "\n") || (i+2 >= len(body) && isRoot && j == len(next.original))
}

// omitRight strips leading whitespace of body[j] if it is content: all of it
// when multiple, else /^[ \t]*\r?\n?/ once.
func omitRight(body []node, j int, multiple bool) {
	if j < 0 || j >= len(body) {
		return
	}
	c, ok := body[j].(*contentNode)
	if !ok || (!multiple && c.rightStripped) {
		return
	}
	v := c.value
	if multiple {
		v = v[skipSpace(v, 0):]
	} else {
		v = strings.TrimLeft(v, " \t")
		v = strings.TrimPrefix(v, "\r")
		v = strings.TrimPrefix(v, "\n")
	}
	c.rightStripped = v != c.value
	c.value = v
}

// omitLeft strips trailing whitespace of body[j] if it is content: all of it
// when multiple, else /[ \t]+$/. It reports whether anything was stripped.
func omitLeft(body []node, j int, multiple bool) bool {
	if j < 0 || j >= len(body) {
		return false
	}
	c, ok := body[j].(*contentNode)
	if !ok || (!multiple && c.leftStripped) {
		return false
	}
	v := strings.TrimRight(c.value, " \t")
	if multiple {
		v = trimRightSpace(c.value)
	}
	c.leftStripped = v != c.value
	c.value = v
	return c.leftStripped
}

// trimRightSpace removes trailing JS \s characters.
func trimRightSpace(s string) string {
	for len(s) > 0 {
		r, w := utf8.DecodeLastRuneInString(s)
		if !isJSSpace(r) {
			break
		}
		s = s[:len(s)-w]
	}
	return s
}
