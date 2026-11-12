package handlebars

// htmlEscapes is the utils.js escapeExpression table, indexed by byte.
var htmlEscapes = [256]string{
	'&':  "&amp;",
	'<':  "&lt;",
	'>':  "&gt;",
	'"':  "&quot;",
	'\'': "&#x27;",
	'`':  "&#x60;",
	'=':  "&#x3D;",
}

// appendEscaped appends s to dst HTML-escaped like escapeExpression,
// copying the runs between escapable bytes in bulk.
func appendEscaped(dst []byte, s string) []byte {
	last := 0
	for i := 0; i < len(s); i++ {
		if e := htmlEscapes[s[i]]; e != "" {
			dst = append(dst, s[last:i]...)
			dst = append(dst, e...)
			last = i + 1
		}
	}
	return append(dst, s[last:]...)
}
