package handlebars

import "strconv"

// specFuncs maps sha1(JS source)[:12] to the Go Helper (or plain value)
// replacing that JS code object in the spec corpus. A nil value counts as
// missing: the test is SKIPped. Get paste-ready stubs with HBSPEC_STUBS=1.
//
// Every JS function is ported as a Helper: JS `this` is o.Ctx(), JS
// arguments are o.Args (plus the options object, o itself).
var specFuncs = map[string]any{
	// basic.json
	"34966f7c73e5": func(o *HelperOptions) (any, error) { // (_undefined === undefined) + ' ' + (_null === null) + ' ' + (typeof options)
		return strconv.FormatBool(arg(o, 0) == nil) + " " + strconv.FormatBool(arg(o, 1) == nil) + " object", nil
	},
	"2edd93d7732a": func(o *HelperOptions) (any, error) { return "undefined!", nil },
	"436d5abc5e20": func(o *HelperOptions) (any, error) { return "null!", nil },
	"d2dff0c29550": func(o *HelperOptions) (any, error) { return SafeString("&'\\<>"), nil },
	"36f51cdf6b6a": func(o *HelperOptions) (any, error) { return "Awesome", nil },
	"93b15e99f30d": func(o *HelperOptions) (any, error) { // this.more
		v, _ := lookupProperty(o.Ctx(), "more")
		return v, nil
	},
	"d695a66f2624": func(o *HelperOptions) (any, error) { return arg(o, 0), nil },      // (context) => context
	"be222c0e1fa5": func(o *HelperOptions) (any, error) { return o.FnWith(arg(o, 0)) }, // options.fn(context)
	"b8a2b81a4a16": func(o *HelperOptions) (any, error) { return o.Fn() },              // options.fn(this)
	"81724630194b": func(o *HelperOptions) (any, error) { return o.Ctx(), nil },        // this
	"95677ba100c6": func(o *HelperOptions) (any, error) { return "bar " + jsStr(arg(o, 0)), nil },
	"5e302cbc7da4": func(o *HelperOptions) (any, error) { return arg(o, 0), nil }, // (value) => value
	"d84b64c3871b": func(o *HelperOptions) (any, error) { return "bar" + jsStr(arg(o, 0)), nil },
	"27589ee64ef5": func(o *HelperOptions) (any, error) { return "bar", nil },
	"4529e62c8f31": func(o *HelperOptions) (any, error) { return arg(o, 0), nil }, // (arg) => arg
}

// arg is JS positional parameter i (undefined when not passed).
func arg(o *HelperOptions, i int) any {
	if i < len(o.Args) {
		return o.Args[i]
	}
	return nil
}

// jsStr is JS String(v) for string concatenation in ported helpers.
func jsStr(v any) string { return string(appendValue(nil, v, false)) }
