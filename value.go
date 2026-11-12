package handlebars

import (
	"math"
	"reflect"
	"strconv"
)

// Dynamic value operations: JS semantics over Go values. JS null/undefined
// are nil (or a nil pointer/map/func/chan), numbers are any Go numeric type
// (the spec harness uses float64), arrays are slices/arrays, objects are
// KeyedCollection, string-keyed maps and structs.

// maxValueDepth bounds pointer chasing and nested-array formatting, so
// self-referential Go values cannot hang or overflow the stack.
const maxValueDepth = 64

// nullCtx is JS container.nullContext: the `this` of a helper called while
// the current context is null/undefined (an empty object).
type nullCtx struct{}

// asHelper reports whether v is a callable function value (JS typeof v ===
// 'function'). The only function shape is Helper.
func asHelper(v any) (Helper, bool) {
	switch f := v.(type) {
	case Helper:
		return f, f != nil
	case func(*HelperOptions) (any, error):
		return f, f != nil
	}
	return nil, false
}

// isNull reports whether v is JS null/undefined.
func isNull(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.IsNil()
	}
	return false
}

// jsTruthy is JS Boolean(v). SafeString is an object in JS, so always truthy.
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && x == x
	case int:
		return x != 0
	case SafeString:
		return true
	}
	if isNull(v) {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.String:
		return rv.Len() > 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		return f != 0 && f == f
	}
	return true
}

// sameContext approximates JS `a == b` as used by wrapProgram to decide
// whether a block context becomes a new depth: null matches null, objects
// compare by identity, primitives by value.
// ponytail: no JS cross-type coercion (1 == "1"); add it if a spec test needs it.
func sameContext(a, b any) bool {
	if isNull(a) || isNull(b) {
		return isNull(a) && isNull(b)
	}
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Type() != rb.Type() {
		return false
	}
	switch ra.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return ra.Pointer() == rb.Pointer()
	case reflect.Slice:
		return ra.Pointer() == rb.Pointer() && ra.Len() == rb.Len()
	}
	return ra.Comparable() && ra.Equal(rb)
}

// lookupProperty is JS container.lookupProperty: own properties only, so a
// missing key, an unexported struct field or a non-string-keyed map all
// yield (nil, false). Strings and arrays have `length`; arrays also have
// canonical numeric indices. It never panics.
func lookupProperty(parent any, name string) (any, bool) {
	switch p := parent.(type) {
	case nil:
		return nil, false
	case KeyedCollection:
		return p.Get(name)
	case map[string]any:
		v, ok := p[name]
		return v, ok
	case []any:
		if name == "length" {
			return len(p), true
		}
		if i, ok := arrayIndex(name, len(p)); ok {
			return p[i], true
		}
		return nil, false
	case string:
		if name == "length" {
			return utf16Len(p), true
		}
		return nil, false
	}
	return lookupReflect(reflect.ValueOf(parent), name)
}

func lookupReflect(rv reflect.Value, name string) (any, bool) {
	for i := 0; rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface; i++ {
		if rv.IsNil() || i == maxValueDepth {
			return nil, false
		}
		rv = rv.Elem()
	}
	var v reflect.Value
	switch rv.Kind() {
	case reflect.Map:
		kt := rv.Type().Key()
		if kt.Kind() != reflect.String {
			return nil, false
		}
		v = rv.MapIndex(reflect.ValueOf(name).Convert(kt))
	case reflect.Struct:
		f, ok := rv.Type().FieldByName(name)
		if !ok || !f.IsExported() {
			return nil, false
		}
		var err error
		if v, err = rv.FieldByIndexErr(f.Index); err != nil { // nil embedded pointer
			return nil, false
		}
	case reflect.Slice, reflect.Array:
		if name == "length" {
			return rv.Len(), true
		}
		if i, ok := arrayIndex(name, rv.Len()); ok {
			v = rv.Index(i)
		}
	case reflect.String:
		if name == "length" {
			return utf16Len(rv.String()), true
		}
	}
	if !v.IsValid() || !v.CanInterface() {
		return nil, false
	}
	return v.Interface(), true
}

// arrayIndex parses a canonical JS array index ("0", "12"; not "01", "-1")
// below n.
func arrayIndex(name string, n int) (int, bool) {
	if name == "" || len(name) > 1 && name[0] == '0' {
		return 0, false
	}
	i := 0
	for j := 0; j < len(name); j++ {
		c := name[j]
		if c < '0' || c > '9' {
			return 0, false
		}
		if i = i*10 + int(c-'0'); i >= n {
			return 0, false
		}
	}
	return i, true
}

// utf16Len is JS String#length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

// appendValue appends JS String(v), HTML-escaped when escape is set; nil
// appends nothing (handlebars `append`/escapeExpression treat null as "").
// The caller handles a top-level SafeString; nested ones are plain strings,
// as in JS where Array#join stringifies them before escaping.
func appendValue(dst []byte, v any, escape bool) []byte {
	switch x := v.(type) {
	case nil:
		return dst
	case string:
		if escape {
			return appendEscaped(dst, x)
		}
		return append(dst, x...)
	case float64:
		return appendJSNumber(dst, x)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case bool:
		return strconv.AppendBool(dst, x)
	}
	return appendReflect(dst, reflect.ValueOf(v), escape, 0)
}

func appendReflect(dst []byte, rv reflect.Value, escape bool, depth int) []byte {
	switch rv.Kind() {
	case reflect.String:
		if escape {
			return appendEscaped(dst, rv.String())
		}
		return append(dst, rv.String()...)
	case reflect.Bool:
		return strconv.AppendBool(dst, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return appendJSNumber(dst, rv.Float())
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() || depth == maxValueDepth {
			return dst
		}
		return appendReflect(dst, rv.Elem(), escape, depth+1)
	case reflect.Slice, reflect.Array: // Array#toString: join(",")
		if depth == maxValueDepth {
			return dst
		}
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendReflect(dst, rv.Index(i), escape, depth+1)
		}
		return dst
	case reflect.Map:
		if rv.IsNil() {
			return dst
		}
		return append(dst, "[object Object]"...)
	case reflect.Struct:
		return append(dst, "[object Object]"...)
	}
	return dst // funcs, chans: no JS equivalent worth printing
}

// appendJSNumber appends f formatted like JS Number.prototype.toString:
// shortest round-trip digits, plain notation for 1e-7 < |f| < 1e21,
// exponent notation ("1e+21", "1e-7") otherwise.
func appendJSNumber(dst []byte, f float64) []byte {
	switch {
	case f != f:
		return append(dst, "NaN"...)
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...)
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...)
	case f == 0: // also -0
		return append(dst, '0')
	}
	if f < 0 {
		dst = append(dst, '-')
		f = -f
	}
	var scratch [32]byte
	e := strconv.AppendFloat(scratch[:0], f, 'e', -1, 64) // d[.ddd]e±XX
	m := 1
	for e[m] != 'e' {
		m++
	}
	exp := 0
	for _, c := range e[m+2:] {
		exp = exp*10 + int(c-'0')
	}
	if e[m+1] == '-' {
		exp = -exp
	}
	digits := e[:1]
	if m > 1 {
		digits = append(digits, e[2:m]...) // drop the '.' in place
	}
	k, n := len(digits), exp+1 // f = 0.digits × 10^n
	switch {
	case k <= n && n <= 21:
		dst = append(dst, digits...)
		for ; k < n; k++ {
			dst = append(dst, '0')
		}
	case 0 < n && n <= 21:
		dst = append(dst, digits[:n]...)
		dst = append(dst, '.')
		dst = append(dst, digits[n:]...)
	case -6 < n && n <= 0:
		dst = append(dst, '0', '.')
		for ; n < 0; n++ {
			dst = append(dst, '0')
		}
		dst = append(dst, digits...)
	default:
		dst = append(dst, digits[0])
		if k > 1 {
			dst = append(dst, '.')
			dst = append(dst, digits[1:]...)
		}
		dst = append(dst, 'e')
		if exp >= 0 {
			dst = append(dst, '+')
		}
		dst = strconv.AppendInt(dst, int64(exp), 10)
	}
	return dst
}
