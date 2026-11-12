package handlebars

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// The builtin helpers port lib/handlebars/helpers/*.js. They are library
// code: the renderer calls them directly (never through s.call) and they
// render straight into the current output.

// builtinHelper is the default helper registry; user registrations shadow it.
func builtinHelper(name string) Helper {
	switch name {
	case "each":
		return eachHelper
	case "if":
		return ifHelper
	case "unless":
		return unlessHelper
	case "with":
		return withHelper
	case "lookup":
		return lookupHelper
	case "log":
		return logHelper
	}
	return nil
}

// callable calls v when it is a function value (JS isFunction(x) ? x.call(this) : x).
func (o *HelperOptions) callable(name string, v any) (any, error) {
	if f, ok := asHelper(v); ok {
		return o.s.call(name, f, &HelperOptions{s: o.s, this: o.this})
	}
	return v, nil
}

func ifHelper(o *HelperOptions) (any, error) {
	if len(o.Args) != 1 {
		return nil, &Error{Message: "#if requires exactly one argument"}
	}
	return nil, o.cond(o.Args[0], o.fn, o.inverse)
}

func unlessHelper(o *HelperOptions) (any, error) {
	if len(o.Args) != 1 {
		return nil, &Error{Message: "#unless requires exactly one argument"}
	}
	// JS calls instance.helpers['if'] with fn and inverse swapped, so a
	// user-registered `if` sees (and may override) unless.
	if h, builtin := o.s.helper("if"); !builtin {
		return o.s.call("if", h, &HelperOptions{Name: o.Name, Args: o.Args, Hash: o.Hash, s: o.s, this: o.this, fn: o.inverse, inverse: o.fn})
	}
	return nil, o.cond(o.Args[0], o.inverse, o.fn)
}

// cond renders yes when v is truthy and non-empty (includeZero: when merely
// non-empty), else no.
func (o *HelperOptions) cond(v any, yes, no *program) error {
	v, err := o.callable("if", v)
	if err != nil {
		return err
	}
	if (!jsTruthy(o.Hash["includeZero"]) && !jsTruthy(v)) || isEmpty(v) {
		return o.s.renderProgram(no, o.this)
	}
	return o.s.renderProgram(yes, o.this)
}

func withHelper(o *HelperOptions) (any, error) {
	if len(o.Args) != 1 {
		return nil, &Error{Message: "#with requires exactly one argument"}
	}
	ctx, err := o.callable("with", o.Args[0])
	if err != nil {
		return nil, err
	}
	if isEmpty(ctx) {
		return nil, o.s.renderProgram(o.inverse, o.this)
	}
	var bp []any
	if o.fn != nil && len(o.fn.blockParams) > 0 {
		bp = []any{ctx}
	}
	return nil, o.s.renderProgramBP(o.fn, ctx, bp)
}

func lookupHelper(o *HelperOptions) (any, error) {
	if len(o.Args) == 0 || !jsTruthy(o.Args[0]) {
		if len(o.Args) == 0 {
			return nil, nil
		}
		return o.Args[0], nil
	}
	field := "undefined"
	if len(o.Args) > 1 {
		field = string(appendValue(nil, o.Args[1], false, 1<<10))
	}
	v, _ := lookupProperty(o.Args[0], field)
	return v, nil
}

var logLevels = [...]string{"debug", "info", "warn", "error"}

// logHelper is helpers/log.js plus logger.js: the level comes from the
// `level` hash arg, else @level, else info; messages below info are dropped.
func logHelper(o *HelperOptions) (any, error) {
	if o.s.t.Logger == nil {
		return nil, nil
	}
	lv := o.Hash["level"]
	if lv == nil {
		if lv = o.DataVar("level"); lv == nil {
			lv = 1
		}
	}
	level, ok := logLevel(lv)
	if !ok || level < 1 { // NaN, or below the default threshold "info"
		return nil, nil
	}
	name := "log" // console[methodMap[level]] is missing
	if level < float64(len(logLevels)) && level == float64(int(level)) {
		name = logLevels[int(level)]
	}
	// The Logger is user code: run it under s.call's recover.
	return o.s.call("log", func(*HelperOptions) (any, error) {
		o.s.t.Logger(name, o.Args...)
		return nil, nil
	}, o)
}

// logLevel is logger.lookupLevel: names map to their index, other strings
// go through parseInt; ok is false for NaN.
func logLevel(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, x == x
	case int:
		return float64(x), true
	case string:
		if i := slices.Index(logLevels[:], strings.ToLower(x)); i >= 0 {
			return float64(i), true
		}
		x = strings.TrimSpace(x)
		end := 0
		for end < len(x) && (x[end] >= '0' && x[end] <= '9' || end == 0 && (x[0] == '-' || x[0] == '+')) {
			end++
		}
		n, err := strconv.Atoi(x[:end])
		return float64(n), err == nil
	}
	return 0, false // ponytail: other types coerce in JS; unneeded
}

// eachRun is the state of one {{#each}}: a single data frame and block
// param slice are overwritten by every iteration, so nothing is allocated
// per item. Nothing may retain them past the iteration.
type eachRun struct {
	o     *HelperOptions
	frame *dataFrame
	bp    []any
	n     int
}

func (r *eachRun) iter(v any, key string, idx int, arr, last bool) error {
	s := r.o.s
	if err := s.step(); err != nil {
		return err
	}
	if f := r.frame; f != nil {
		f.key, f.arr, f.index, f.first, f.last = key, arr, idx, r.n == 0, last
	}
	if r.bp != nil {
		r.bp[0] = v
		if arr {
			r.bp[1] = idx
		} else {
			r.bp[1] = key
		}
	}
	r.n++
	return s.renderFrame(r.o.fn, v, r.bp, r.frame)
}

// eachHelper is helpers/each.js. As in JS only objects iterate (arrays,
// maps, KeyedCollections, structs); strings and numbers take the else block.
func eachHelper(o *HelperOptions) (any, error) {
	if len(o.Args) == 0 {
		return nil, &Error{Message: "Must pass iterator to #each"}
	}
	ctx, err := o.callable("each", o.Args[0])
	if err != nil {
		return nil, err
	}
	r := eachRun{o: o}
	if o.s.data != nil {
		r.frame = &dataFrame{parent: o.s.data, loop: true}
	}
	if o.fn != nil && len(o.fn.blockParams) > 0 {
		r.bp = make([]any, 2)
	}
	if !isNull(ctx) {
		err = forOwn(ctx, r.iter)
	}
	if err == nil && r.n == 0 {
		err = o.s.renderProgram(o.inverse, o.this)
	}
	return nil, err
}

// forOwn visits the own enumerable properties of ctx in JS order, as
// {{#each}} and Utils.extend see them: array items (key "", arr true),
// object keys (KeyedCollection order, maps sorted, struct fields).
func forOwn(ctx any, visit func(v any, key string, idx int, arr, last bool) error) error {
	switch x := ctx.(type) {
	case []any:
		for i, v := range x {
			if v == (hole{}) {
				continue
			}
			if err := visit(v, "", i, true, i == len(x)-1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		return visitKeys(sortedKeys(x), func(k string) any { return x[k] }, visit)
	case KeyedCollection:
		return visitKeys(x.Keys(), func(k string) any { v, _ := x.Get(k); return v }, visit)
	}
	rv := reflect.ValueOf(ctx)
	for i := 0; i < 8 && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface); i++ {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i, n := 0, rv.Len(); i < n; i++ {
			if err := visit(rv.Index(i).Interface(), "", i, true, i == n-1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil
		}
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		slices.Sort(keys)
		kt := rv.Type().Key()
		return visitKeys(keys, func(k string) any { return rv.MapIndex(reflect.ValueOf(k).Convert(kt)).Interface() }, visit)
	case reflect.Struct:
		t := rv.Type()
		var names []string
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				names = append(names, t.Field(i).Name)
			}
		}
		return visitKeys(names, func(k string) any { return rv.FieldByName(k).Interface() }, visit)
	}
	return nil
}

func visitKeys(keys []string, get func(string) any, visit func(v any, key string, idx int, arr, last bool) error) error {
	for i, k := range keys {
		if err := visit(get(k), k, i, false, i == len(keys)-1); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
