package handlebars

import (
	"math"
	"reflect"
	"strconv"
	"sync"
)

// Dynamic value operations: JS semantics over Go values. JS null/undefined
// are nil (or a nil pointer/map/func/chan), numbers are any Go numeric type
// (the spec harness uses float64), arrays are slices/arrays, objects are
// KeyedCollection, string-keyed maps and structs.

// maxValueDepth bounds pointer chasing and nested-array recursion depth
// (stack use). Fan-out (shared or cyclic arrays) is bounded separately by
// appendValue's output limit.
const maxValueDepth = 64

// nullCtx is JS container.nullContext: the `this` of a helper called while
// the current context is null/undefined (an empty object).
type nullCtx struct{}

// hole is a missing element of a sparse []any (JS `i in arr` false). Only
// test harnesses create it; each skips it, lookup yields undefined, join
// renders it empty.
// ponytail: sentinel in []any instead of a sparse type; holes inside other
// slice types are not modelled.
type hole struct{}

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
	case reflect.Pointer: // followed, as by lookups and printing
		switch e := deref(rv); e.Kind() {
		case reflect.Invalid:
			return false
		case reflect.Struct, reflect.Array: // objects: truthy, and not copied out
			return true
		default:
			return jsTruthy(e.Interface())
		}
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

// isEmpty is utils.js isEmpty: falsy values other than the number 0, and
// empty arrays (Go slices/arrays). Maps and KeyedCollections are objects,
// so never empty.
func isEmpty(v any) bool {
	if !jsTruthy(v) {
		if isNull(v) {
			return true
		}
		switch rv := deref(reflect.ValueOf(v)); rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return rv.Int() != 0
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return rv.Uint() != 0
		case reflect.Float32, reflect.Float64:
			return rv.Float() != 0 // NaN is falsy and not 0: empty
		}
		return true
	}
	rv := deref(reflect.ValueOf(v))
	return (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) && rv.Len() == 0
}

// sameContext approximates JS `a == b` as used by wrapProgram to decide
// whether a block context becomes a new depth: null matches null, objects
// compare by identity, primitives by value. Arrays and structs held by
// value have no identity, and comparing their contents costs their size on
// every block entry, so only zero-size ones (nullCtx) match.
// ponytail: no JS cross-type coercion (1 == "1"); add it if a spec test needs it.
// ponytail: re-entering the same array or struct value pushes a depth, so
// ../ counts it once more than JS would; pointers keep exact identity.
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
	case reflect.Array, reflect.Struct:
		return ra.Type().Size() == 0
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
		if isNull(p) { // typed nil, e.g. (*T)(nil): Get would deref a nil receiver
			return nil, false
		}
		return p.Get(name)
	case map[string]any:
		v, ok := p[name]
		return v, ok
	case []any:
		if name == "length" {
			return len(p), true
		}
		if i, ok := arrayIndex(name, len(p)); ok && p[i] != (hole{}) {
			return p[i], true
		}
		return nil, false
	case string:
		if name == "length" {
			return utf16Len(p), true
		}
		return nil, false
	}
	return lookupReflect(deref(reflect.ValueOf(parent)), name)
}

// deref follows pointers and interfaces to the value they hold, without
// copying it; nil, or a chain longer than maxValueDepth, is the zero Value.
func deref(rv reflect.Value) reflect.Value {
	for i := 0; rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface; i++ {
		if rv.IsNil() || i == maxValueDepth {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	return rv
}

// structFields is the exported fields of a struct type that lookups and
// {{#each}} read. FieldByName scans every field, unexported ones too, on
// each call, a cost no op count bounds; so, like encoding/json, they are
// collected once per type.
type structFields struct {
	byName map[string][]int // fields FieldByName finds, promoted ones too
	names  []string         // own fields in order: {{#each}}'s keys
	own    []int            // field index of names[i]
}

var structCache sync.Map // reflect.Type -> *structFields

func fieldsOf(t reflect.Type) *structFields {
	if fs, ok := structCache.Load(t); ok {
		return fs.(*structFields)
	}
	fs := &structFields{byName: map[string][]int{}}
	for _, f := range reflect.VisibleFields(t) { // what FieldByName finds
		if !f.IsExported() {
			continue
		}
		fs.byName[f.Name] = f.Index
		if len(f.Index) == 1 {
			fs.names, fs.own = append(fs.names, f.Name), append(fs.own, f.Index[0])
		}
	}
	got, _ := structCache.LoadOrStore(t, fs)
	return got.(*structFields)
}

func lookupReflect(rv reflect.Value, name string) (any, bool) {
	var v reflect.Value
	switch rv.Kind() {
	case reflect.Map:
		kt := rv.Type().Key()
		if kt.Kind() != reflect.String {
			return nil, false
		}
		v = rv.MapIndex(reflect.ValueOf(name).Convert(kt))
	case reflect.Struct:
		idx, ok := fieldsOf(rv.Type()).byName[name]
		if !ok {
			return nil, false
		}
		var err error
		if v, err = rv.FieldByIndexErr(idx); err != nil { // nil embedded pointer
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
// Array formatting stops once len(dst) > limit (the caller's remaining
// output budget), so a shared or self-referential array costs at most
// O(limit × maxValueDepth) work; the caller then reports the overflow.
func appendValue(dst []byte, v any, escape bool, limit int) []byte {
	switch x := v.(type) {
	case nil:
		return dst
	case string:
		return appendString(dst, x, escape, limit)
	case float64:
		return appendJSNumber(dst, x)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case bool:
		return strconv.AppendBool(dst, x)
	}
	return appendReflect(dst, reflect.ValueOf(v), escape, limit, 0)
}

func appendReflect(dst []byte, rv reflect.Value, escape bool, limit, depth int) []byte {
	if len(dst) > limit {
		return dst
	}
	switch rv.Kind() {
	case reflect.String:
		return appendString(dst, rv.String(), escape, limit)
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
		return appendReflect(dst, rv.Elem(), escape, limit, depth+1)
	case reflect.Slice, reflect.Array: // Array#toString: join(",")
		if depth == maxValueDepth {
			return dst
		}
		for i := 0; i < rv.Len() && len(dst) <= limit; i++ {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendReflect(dst, rv.Index(i), escape, limit, depth+1)
		}
		return dst
	case reflect.Map:
		if rv.IsNil() {
			return dst
		}
		return append(dst, "[object Object]"...)
	case reflect.Struct:
		if rv.Type() == reflect.TypeOf(hole{}) {
			return dst
		}
		return append(dst, "[object Object]"...)
	}
	return dst // funcs, chans: no JS equivalent worth printing
}

// appendString appends s, HTML-escaped when escape is set, but no more of
// it than takes dst past limit: a long string costs the output budget, not
// its length (escaping still writes up to 6 bytes per byte it takes).
func appendString(dst []byte, s string, escape bool, limit int) []byte {
	if room := limit - len(dst); room < len(s) { // limit+1 could overflow
		s = s[:max(room+1, 0)]
	}
	if escape {
		return appendEscaped(dst, s)
	}
	return append(dst, s...)
}

// appendJSNumber appends f formatted like JS Number.prototype.toString:
// shortest round-trip digits, plain notation for 1e-6 <= |f| < 1e21,
// exponent notation ("1e+21", "1e-7") otherwise.
func appendJSNumber(dst []byte, f float64) []byte {
	switch abs := math.Abs(f); {
	case f != f:
		return append(dst, "NaN"...)
	case math.IsInf(f, 0):
		if f < 0 {
			dst = append(dst, '-')
		}
		return append(dst, "Infinity"...)
	case f == 0: // also -0
		return append(dst, '0')
	case 1e-6 <= abs && abs < 1e21: // the range JS prints plainly
		return strconv.AppendFloat(dst, f, 'f', -1, 64)
	}
	dst = strconv.AppendFloat(dst, f, 'e', -1, 64) // d[.ddd]e±dd[d]
	if n := len(dst); dst[n-2] == '0' && (dst[n-3] == '+' || dst[n-3] == '-') {
		dst[n-2], dst = dst[n-1], dst[:n-1] // JS has no exponent padding: e-7, not e-07
	}
	return dst
}
