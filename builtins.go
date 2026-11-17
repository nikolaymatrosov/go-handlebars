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
		// Args is cloned: o, which backs it, is recycled after the call.
		return o.s.call("if", h, &HelperOptions{Name: o.Name, Args: slices.Clone(o.Args), Hash: o.Hash, s: o.s, this: o.this, fn: o.inverse, inverse: o.fn})
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
	var df *dataFrame
	id, tracked := o.id0()
	if tracked && o.s.data != nil { // data.contextPath = appendContextPath(...)
		path, err := o.s.appendContextPath(id)
		if err != nil {
			return nil, err
		}
		df = &dataFrame{parent: o.s.data, contextPath: path}
	}
	var bp bpFrame
	if o.fn != nil && len(o.fn.blockParams) > 0 {
		bp.vals = []any{ctx}
		if tracked { // blockParams.path = [data && data.contextPath]
			path := "undefined"
			if df != nil {
				path = df.contextPath
			}
			bp.ids = []string{path}
		}
	}
	return nil, o.s.renderFrame(o.fn, ctx, bp, df)
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
		switch k := o.Args[1].(type) {
		case string: // used as is, not copied
			field = k
		case int: // @index: Itoa allocates nothing below 100, one string above
			field = strconv.Itoa(k)
		default:
			field = string(appendValue(nil, k, false, 1<<10))
		}
	}
	if err := o.s.steps(len(field) >> 10); err != nil { // hashed by the read, even from the data
		return nil, err
	}
	v, _ := o.s.prop(o.Args[0], field)
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
	if str, ok := lv.(string); ok { // logLevel lowercases, trims and parses it
		if err := o.s.steps(len(str) >> 10); err != nil {
			return nil, err
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
	// The Logger is user code: run it under s.call's recover. It gets a
	// copy of the args, not o (recycled once log returns), so it may keep
	// them and the state stays poolable.
	args := slices.Clone(o.Args)
	return o.s.call("log", func(*HelperOptions) (any, error) {
		o.s.t.Logger(name, args...)
		return nil, nil
	}, nil)
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
		// Only ASCII letters and the 3-byte Kelvin sign lowercase to the
		// letters of a level name, so a longer string can't match one: it
		// isn't lowercased (copied).
		if len(x) <= 32 {
			if i := slices.Index(logLevels[:], strings.ToLower(x)); i >= 0 {
				return float64(i), true
			}
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
// per item (but trackIds' item path). Nothing may retain them past the
// iteration; runs are recycled (renderState.freeRuns), and hold the frame
// and block params themselves, so a loop allocates nothing at all.
type eachRun struct {
	o     *HelperOptions
	frame *dataFrame
	bp    []any
	n     int
	// trackIds: an item's path is JS contextPath + field, built in path
	// after its first pathLen bytes (the contextPath); ids is the block
	// params' path, [contextPath + field, null].
	path    []byte // nil: not tracking
	pathLen int
	ids     []string

	frameBuf dataFrame // frame, when data is on
	bpBuf    [2]any    // bp: the item and its key or index
}

func (r *eachRun) iter(v any, key string, idx int, arr, last bool) error {
	s := r.o.s
	if err := s.step(); err != nil {
		return err
	}
	if f := r.frame; f != nil {
		f.key, f.arr, f.index, f.first, f.last = key, arr, idx, idx == 0, last
	}
	if r.bp != nil {
		r.bp[0] = v
		switch {
		case len(r.bp) == 1: // `as |item|`: no key or index to box
		case arr:
			r.bp[1] = idx
		default:
			r.bp[1] = key
		}
	}
	if r.path != nil {
		if arr {
			r.path = strconv.AppendInt(r.path[:r.pathLen], int64(idx), 10)
		} else {
			r.path = append(r.path[:r.pathLen], key...)
		}
		if err := chargePath(s, r.path); err != nil { // copied per item
			return err
		}
		path := string(r.path)
		if r.frame != nil {
			r.frame.contextPath = path
		}
		if r.ids != nil {
			r.ids[0] = path
		}
	}
	r.n++
	return s.renderFrame(r.o.fn, v, bpFrame{r.bp, r.ids}, r.frame)
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
	r := o.s.freeRuns.get()
	defer o.s.freeRuns.put(r)
	r.o = o
	if o.s.data != nil {
		r.frameBuf = dataFrame{parent: o.s.data, loop: true}
		r.frame = &r.frameBuf
	}
	if o.fn != nil && len(o.fn.blockParams) > 0 {
		n := min(len(o.fn.blockParams), 2)
		r.bp = r.bpBuf[:n:n]
	}
	if id, ok := o.id0(); ok { // trackIds
		r.path = []byte("undefined") // without data JS leaves contextPath undefined: undefined + field
		if r.frame != nil {
			path, err := o.s.appendContextPath(id)
			if err != nil {
				return nil, err
			}
			r.path = append([]byte(path), '.')
			if err := chargePath(o.s, r.path); err != nil { // copied per each call, items or not
				return nil, err
			}
		}
		r.pathLen = len(r.path)
		if r.bp != nil {
			r.ids = []string{"", "null"}
		}
	}
	if !isNull(ctx) {
		err = forOwn(o.s, ctx, r.iter)
	}
	if err == nil && r.n == 0 {
		err = o.s.renderProgram(o.inverse, o.this)
	}
	return nil, err
}

// forOwn visits the own enumerable properties of ctx in JS order, as
// {{#each}} and Utils.extend see them: array items (key "", arr true),
// object keys (KeyedCollection order, maps sorted, struct fields).
func forOwn(s *renderState, ctx any, visit func(v any, key string, idx int, arr, last bool) error) error {
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
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		return visitKeys(s, keys, true, func(_ int, k string) any { return x[k] }, visit)
	case KeyedCollection:
		return visitKeys(s, x.Keys(), false, func(_ int, k string) any { v, _ := x.Get(k); return v }, visit)
	}
	switch rv := deref(reflect.ValueOf(ctx)); rv.Kind() {
	case reflect.Slice, reflect.Array:
		per := copyOps(rv.Type().Elem()) // Interface copies each item; unchecked, as iter steps
		for i, n := 0, rv.Len(); i < n; i++ {
			s.ops += per
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
		kt, per := rv.Type().Key(), copyOps(rv.Type().Elem())
		return visitKeys(s, keys, true, func(_ int, k string) any {
			if v := rv.MapIndex(reflect.ValueOf(k).Convert(kt)); v.IsValid() {
				s.ops += per
				return v.Interface()
			}
			return nil // deleted mid-loop by a helper: JS context[field] is undefined
		}, visit)
	case reflect.Struct:
		fs := fieldsOf(rv.Type())
		return visitKeys(s, fs.names, false, func(i int, _ string) any {
			f := rv.Field(fs.own[i])
			s.ops += copyOps(f.Type())
			return f.Interface()
		}, visit)
	}
	return nil
}

// visitKeys visits keys, sorted first if sort. Before the first visit it
// charges an op per 8 keys, collected whether or not the loop gets past
// its first item, and, before sorting, an op per KiB of all the keys: a
// sort reads each O(log n) times. Each key also costs an op per KiB, as
// get hashes it again.
func visitKeys(s *renderState, keys []string, sort bool, get func(i int, k string) any, visit func(v any, key string, idx int, arr, last bool) error) error {
	n := len(keys) >> 3
	if sort {
		size := 0
		for _, k := range keys {
			size += len(k)
		}
		n += size >> 10
	}
	if err := s.steps(n); err != nil {
		return err
	}
	if sort {
		slices.Sort(keys)
	}
	for i, k := range keys {
		if err := s.steps(len(k) >> 10); err != nil {
			return err
		}
		if err := visit(get(i, k), k, i, false, i == len(keys)-1); err != nil {
			return err
		}
	}
	return nil
}
