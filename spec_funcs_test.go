package handlebars

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

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
	"bff21194b3d3": func(o *HelperOptions) (any, error) { return "hello", nil },   // function () { return 'hello'; }

	// builtins.json
	"02fce0ddd099": func(o *HelperOptions) (any, error) { v, _ := lookupProperty(o.Ctx(), "world"); return v, nil }, // function () {return this.world; }
	"c192cfbc3fbb": func(o *HelperOptions) (any, error) { v, _ := lookupProperty(o.Ctx(), "foo"); return v, nil },   // function () {return this.foo; }
	"b774e7e22542": func(o *HelperOptions) (any, error) { // function () { return {first: 'Alan', last: 'Johnson'}; }
		return map[string]any{"first": "Alan", "last": "Johnson"}, nil
	},
	"70db6219bbbf": func(o *HelperOptions) (any, error) { return o.DataVar("exclaim"), nil }, // function(options) { return options.data && options.data.exclaim; }
	"146425d52bb0": func(o *HelperOptions) (any, error) { // function () { return [{text: 'goodbye'}, {text: 'Goodbye'}, {text: 'GOODBYE'}]; }
		return []any{map[string]any{"text": "goodbye"}, map[string]any{"text": "Goodbye"}, map[string]any{"text": "GOODBYE"}}, nil
	},

	// regressions.json
	"c1411e25d8f0": func(o *HelperOptions) (any, error) { return "blah", nil },                   // function () { return 'blah'; }
	"62102493c36e": func(o *HelperOptions) (any, error) { return o.FnWith("") },                  // function (block) { return block.fn(''); }
	"502dcb8e3d45": func(o *HelperOptions) (any, error) { s, err := o.Inverse(); return s, err }, // function (block) { return block.inverse(''); }
	"24e968a2fd90": func(o *HelperOptions) (any, error) { return jsStr(arg(o, 0)), nil },         // function (value) { return value + ''; }
	"f1328be48878": func(o *HelperOptions) (any, error) { // function(conditional, options) { if (conditional) return options.fn(this); else return options.inverse(this); }
		if jsTruthy(arg(o, 0)) {
			return o.Fn()
		}
		return o.Inverse()
	},
	"be4daadd8d6d": func(o *HelperOptions) (any, error) { return o.Hash["length"], nil },                // function(options) { return options.hash.length; }
	"ee7950d75856": func(o *HelperOptions) (any, error) { s, err := o.Fn(); return SafeString(s), err }, // function (options) { return new Handlebars.SafeString(options.fn()); }

	// blocks.json decorators
	"d6b048a79aec": func(o *HelperOptions) (any, error) { return fnProp(o, "run"), nil }, // function (options) { return options.fn.run; }
	"80779a684743": func(o *HelperOptions) (any, error) { // function (options) { return options.fn() + options.fn.run; }
		s, err := o.FnWith(nil)
		return s + jsStr(fnProp(o, "run")), err
	},
	// The decorators, JS (fn, props, container, options): fn.x and props.x
	// are both props[x]; a decorator's o.Fn() is JS options.fn().
	"dfed9ef14eed": func(props map[string]any, o *HelperOptions) error { props["run"] = "success"; return nil }, // fn.run = 'success'; return fn;
	"0a1b4528b501": func(props map[string]any, o *HelperOptions) error { props["run"] = "cess"; return nil },    // fn.run = 'cess';
	"88bcb710b337": func(props map[string]any, o *HelperOptions) error { // fn.run = options.fn(); return fn;
		s, err := o.Fn()
		props["run"] = s
		return err
	},
	"43975141e78f": func(props map[string]any, o *HelperOptions) error { // fn.run = options.fn.nested + options.fn(); return fn;
		s, err := o.Fn()
		props["run"] = jsStr(fnProp(o, "nested")) + s
		return err
	},
	"e70f1e5cf207": func(props map[string]any, o *HelperOptions) error { // props.nested = options.fn();
		s, err := o.Fn()
		props["nested"] = s
		return err
	},
	"449b4a7c55f3": func(props map[string]any, o *HelperOptions) error { // fn.run = (fn.run || '') + options.fn(); return fn;
		s, err := o.Fn()
		prev := ""
		if jsTruthy(props["run"]) {
			prev = jsStr(props["run"])
		}
		props["run"] = prev + s
		return err
	},
	"60637683e7a9": func(props map[string]any, o *HelperOptions) error { props["run"] = o.Args; return nil }, // fn.run = options.args; return fn;
	"680cabb587ba": func(props map[string]any, o *HelperOptions) error { // equals(options.args[0], 'success'); run = true; return fn;
		return equalsArg0(o, "success")
	},
	"b0232b97c13f": func(props map[string]any, o *HelperOptions) error { // equals(options.args[0], undefined); global.run = true; return fn;
		return equalsArg0(o, nil)
	},

	// strict.json
	"cdf6591d4600": func(o *HelperOptions) (any, error) { // equals('value' in options.hash, true); equals(options.hash.value, undefined); return 'success'
		if v, ok := o.Hash["value"]; !ok || v != nil {
			return nil, errors.New("expected hash.value present and undefined")
		}
		return "success", nil
	},

	// data.json
	"4a8f31bb6da2": func(o *HelperOptions) (any, error) { // options.data.adjective + ' ' + this.noun
		n, _ := lookupProperty(o.Ctx(), "noun")
		return jsStr(o.DataVar("adjective")) + " " + jsStr(n), nil
	},
	"2e84d414a45d": func(o *HelperOptions) (any, error) { return fnData(o, o.Ctx(), o.Hash) }, // createFrame(options.data) + hash; options.fn(this, {data: frame})
	"a64f59bdf33f": func(o *HelperOptions) (any, error) { return "Hello " + jsStr(arg(o, 0)), nil },
	"4fcdead63757": func(o *HelperOptions) (any, error) { return "Hello " + jsStr(o.Hash["noun"]), nil },
	"76e25fcfe089": func(o *HelperOptions) (any, error) { // options.data.adjective + ' ' + noun + (this.exclaim ? '!' : '')
		return jsStr(o.DataVar("adjective")) + " " + jsStr(arg(o, 0)) + exclaim(o, "!"), nil
	},
	"0d2d26aa3b1f": func(o *HelperOptions) (any, error) {
		return jsStr(o.DataVar("adjective")) + " world" + exclaim(o, "!"), nil
	},
	"c24902ec9978": func(o *HelperOptions) (any, error) { return o.FnWith(map[string]any{"exclaim": "?"}) }, // options.fn({exclaim: '?'})
	"c06549cee712": func(o *HelperOptions) (any, error) { // options.data.adjective + ' ' + thing + (this.exclaim || '')
		e, _ := lookupProperty(o.Ctx(), "exclaim")
		if !jsTruthy(e) {
			e = ""
		}
		return jsStr(o.DataVar("adjective")) + " " + jsStr(arg(o, 0)) + jsStr(e), nil
	},
	"4c151124c14c": func(o *HelperOptions) (any, error) { // options.data.accessData + ' ' + options.fn({exclaim: '?'})
		s, err := o.FnWith(map[string]any{"exclaim": "?"})
		return jsStr(o.DataVar("accessData")) + " " + s, err
	},
	"3c96f81880a8": func(o *HelperOptions) (any, error) { // options.fn({exclaim: '?', zomg: 'world'}, {data: {adjective: 'sad'}})
		return fnData(o, map[string]any{"exclaim": "?", "zomg": "world"}, map[string]any{"adjective": "sad"})
	},
	"d9d4ea563dd4": func(o *HelperOptions) (any, error) { // options.fn({exclaim: '?'}, {data: {adjective: 'sad'}})
		return fnData(o, map[string]any{"exclaim": "?"}, map[string]any{"adjective": "sad"})
	},
	"7da82310d17e": func(o *HelperOptions) (any, error) { // frame.depth = options.data.depth + 1; options.fn(this, {data: frame})
		d, _ := o.DataVar("depth").(float64)
		return fnData(o, o.Ctx(), map[string]any{"depth": d + 1})
	},

	// helpers.json
	"1547467c4929": func(o *HelperOptions) (any, error) { // function (prefix) {
		return `<a href="` + jsStr(arg(o, 0)) + "/" + jsStr(o.Lookup("url")) + `">` + jsStr(o.Lookup("text")) + "</a>", nil
	},
	"2f3f9e3eb4a9": func(o *HelperOptions) (any, error) { return o.FnWith(nil) }, // function (options) {
	"0df91e02a16b": func(o *HelperOptions) (any, error) { // function (a, b, c, options) {
		s, err := o.FnWith(nil)
		return s + jsStr(arg(o, 0)) + jsStr(arg(o, 1)) + jsStr(arg(o, 2)), err
	},
	"6d8e294dfae8": func(o *HelperOptions) (any, error) { // function (options) {
		out := ""
		for _, bye := range []string{"Goodbye", "goodbye", "GOODBYE"} {
			s, err := o.Fn()
			if err != nil {
				return nil, err
			}
			out += bye + " " + s + "! "
		}
		return out, nil
	},
	"3db93e806b5d": func(o *HelperOptions) (any, error) { // function (options) {
		out := ""
		for _, bye := range []string{"Goodbye", "goodbye", "GOODBYE"} {
			s, err := o.FnWith(map[string]any{})
			if err != nil {
				return nil, err
			}
			out += bye + " " + s + "! "
		}
		return out, nil
	},
	"3d74af472365": func(o *HelperOptions) (any, error) { // function (prefix, options) {
		s, err := o.Fn()
		return `<a href="` + jsStr(arg(o, 0)) + "/" + jsStr(o.Lookup("url")) + `">` + s + "</a>", err
	},
	"d745896a05f7": func(o *HelperOptions) (any, error) { return nil, nil }, // function () {}
	"91dffbb1974f": func(o *HelperOptions) (any, error) { // function (options) { return options.fn({text: 'GOODBYE'}); }
		return o.FnWith(map[string]any{"text": "GOODBYE"})
	},
	"77dd03499b04": func(o *HelperOptions) (any, error) { // function (options) { return '<form>' + options.fn(this) + '</form>'; }
		s, err := o.Fn()
		return "<form>" + s + "</form>", err
	},
	"0254e29c0979": func(o *HelperOptions) (any, error) { // function link(options) {
		s, err := o.Fn()
		return `<a href="/people/` + jsStr(o.Lookup("id")) + `">` + s + "</a>", err
	},
	"de0298484662": func(o *HelperOptions) (any, error) { // function (context, options) { return '<form>' + options.fn(context) + '</form>'; }
		s, err := o.FnWith(arg(o, 0))
		return "<form>" + s + "</form>", err
	},
	"74152a8e0c5d": func(o *HelperOptions) (any, error) { // function (options) { return '<a href="' + this.name + '">' + options.fn(this) + '</a>'; }
		s, err := o.Fn()
		return `<a href="` + jsStr(o.Lookup("name")) + `">` + s + "</a>", err
	},
	"66cc404fc016": func(o *HelperOptions) (any, error) { // function list(context, options) {
		if context, _ := arg(o, 0).([]any); len(context) > 0 {
			out := "<ul>"
			for _, c := range context {
				s, err := o.FnWith(c)
				if err != nil {
					return nil, err
				}
				out += "<li>" + s + "</li>"
			}
			return out + "</ul>", nil
		}
		s, err := o.Inverse()
		return "<p>" + s + "</p>", err
	},
	"93fb762c2375": func(o *HelperOptions) (any, error) { return "winning", nil },   // function () {
	"4d8f14da72db": func(o *HelperOptions) (any, error) { return "fail", nil },      // function () { return 'fail'; }
	"47a2a32157eb": func(o *HelperOptions) (any, error) { return "world", nil },     // function () { return 'world'; }
	"b78444d02e2f": func(o *HelperOptions) (any, error) { return "helpers", nil },   // function () { return 'helpers'; }
	"655b5286d891": func(o *HelperOptions) (any, error) { return "helper", nil },    // function () { return 'helper'; }
	"1169db7cd5e3": func(o *HelperOptions) (any, error) { return "found it!", nil }, // function () { return 'found it!'; }
	"b0aa88072966": func(o *HelperOptions) (any, error) { return "world!", nil },    // function () { return 'world!'; }
	"5e063064b61b": func(o *HelperOptions) (any, error) { // function (times, times2) {
		times, times2 := arg(o, 0), arg(o, 1)
		if jsTypeof(times) != "number" {
			times = "NaN"
		}
		if jsTypeof(times2) != "number" {
			times2 = "NaN"
		}
		return "Hello " + jsStr(times) + " " + jsStr(times2) + " times", nil
	},
	"242a92056ece": func(o *HelperOptions) (any, error) { // function (times) {
		times := arg(o, 0)
		if jsTypeof(times) != "number" {
			times = "NaN"
		}
		return "Hello " + jsStr(times) + " times", nil
	},
	"0274afe23e83": func(o *HelperOptions) (any, error) { // function (param, times, bool1, bool2) {
		times, bool1, bool2 := arg(o, 1), arg(o, 2), arg(o, 3)
		if jsTypeof(times) != "number" {
			times = "NaN"
		}
		if jsTypeof(bool1) != "boolean" {
			bool1 = "NaB"
		}
		if jsTypeof(bool2) != "boolean" {
			bool2 = "NaB"
		}
		return "Hello " + jsStr(arg(o, 0)) + " " + jsStr(times) + " times: " + jsStr(bool1) + " " + jsStr(bool2), nil
	},
	"00f663977c4d": func(o *HelperOptions) (any, error) { return "Hello " + jsStr(arg(o, 0)), nil }, // function (param) { return 'Hello ' + param; }
	"346e103de23a": func(o *HelperOptions) (any, error) { // function (cruel, world) { return 'Goodbye ' + cruel + ' ' + world; }
		return "Goodbye " + jsStr(arg(o, 0)) + " " + jsStr(arg(o, 1)), nil
	},
	"40addaef1c10": func(o *HelperOptions) (any, error) { // function (cruel, world, options) {
		return o.FnWith(map[string]any{"greeting": "Goodbye", "adj": arg(o, 0), "noun": arg(o, 1)})
	},
	"da336151febc": func(o *HelperOptions) (any, error) { // function (options) {
		return "GOODBYE " + jsStr(o.Hash["cruel"]) + " " + jsStr(o.Hash["world"]) + " " + jsStr(o.Hash["times"]) + " TIMES", nil
	},
	"cc4dcfc865d4": func(o *HelperOptions) (any, error) { // function goodbye(options) {
		switch o.Hash["print"] {
		case true:
			return "GOODBYE " + jsStr(o.Hash["cruel"]) + " " + jsStr(o.Hash["world"]), nil
		case false:
			return "NOT PRINTING", nil
		}
		return "THIS SHOULD NOT HAPPEN", nil
	},
	"66bebb7e294a": func(o *HelperOptions) (any, error) { // function (options) {
		s, err := o.Fn()
		return "GOODBYE " + jsStr(o.Hash["cruel"]) + " " + s + " " + jsStr(o.Hash["times"]) + " TIMES", err
	},
	"e1ef1d2bb386": func(o *HelperOptions) (any, error) { // function goodbye(options) {
		switch o.Hash["print"] {
		case true:
			s, err := o.Fn()
			return "GOODBYE " + jsStr(o.Hash["cruel"]) + " " + s, err
		case false:
			return "NOT PRINTING", nil
		}
		return "THIS SHOULD NOT HAPPEN", nil
	},
	"89f0710c9760": func(o *HelperOptions) (any, error) { // function (mesg, options) {
		if o.Name == "link_to" {
			return SafeString("<a>" + jsStr(arg(o, 0)) + "</a>"), nil
		}
		return nil, nil
	},
	"9c74060d6d5a": func(o *HelperOptions) (any, error) { // function (options) {
		if o.Name == "link_to" {
			return SafeString("<a>winning</a>"), nil
		}
		return nil, nil
	},
	"1c7c5e685ba7": func(o *HelperOptions) (any, error) { return "foo", nil },               // function () {
	"b3ffb642a1c8": func(o *HelperOptions) (any, error) { return jsTypeof(arg(o, 0)), nil }, // function (arg) { return typeof arg; }
	"a45318f2a241": func(o *HelperOptions) (any, error) { return o.Lookup("bar"), nil },     // function () { return this.bar; }
	"b1c3e3aaf024": func(o *HelperOptions) (any, error) { return true, nil },                // function () {
	"8394317f1e5a": func(o *HelperOptions) (any, error) { // function () { return this.truthiness(); }
		f, ok := o.Lookup("truthiness").(func(*HelperOptions) (any, error))
		if !ok {
			return nil, errors.New("TypeError: this.truthiness is not a function")
		}
		return f(o) // same `this`; o carries no args here
	},
	"81c9972a87a7": func(o *HelperOptions) (any, error) { return false, nil },                       // function () { return false; }
	"2f8bfd3490c6": func(o *HelperOptions) (any, error) { return "missing: " + o.Name, nil },        // function () {
	"52b8ca3163ef": func(o *HelperOptions) (any, error) { return "helper missing: " + o.Name, nil }, // function () {
	"202bbf4e6630": func(o *HelperOptions) (any, error) { return "ran: " + o.Name, nil },            // function () {
	"4950e7cfad7f": func(o *HelperOptions) (any, error) { return toUpperCase(o.Lookup("goodbye")) }, // function () {
	"8ca05d61e43e": func(o *HelperOptions) (any, error) { // function (world) {
		s, err := toUpperCase(arg(o, 0))
		return "cruel " + s, err
	},
	"999d9735953e": func(o *HelperOptions) (any, error) { // function (options) {
		u, err := toUpperCase(o.Lookup("goodbye"))
		if err != nil {
			return nil, err
		}
		s, err := o.Fn()
		return u + s, err
	},
	"f6c577395672": func(o *HelperOptions) (any, error) { // function (options) {
		if err := equalsBlockParams(o, 1); err != nil {
			return nil, err
		}
		return fnBP(o, map[string]any{"value": "bar"}, []any{1.0, 2.0})
	},
	"b7d0e8a36c91": func(o *HelperOptions) (any, error) { // function (options) {
		if err := equalsBlockParams(o, 1); err != nil {
			return nil, err
		}
		return fnBP(o, map[string]any{}, []any{1.0, 2.0})
	},
	"784807fb6b10": func(o *HelperOptions) (any, error) { // function (options) {
		if err := equalsBlockParams(o, 1); err != nil {
			return nil, err
		}
		return fnBP(o, o.Ctx(), []any{1.0, 2.0})
	},
	"96c927a86ce3": func(o *HelperOptions) (any, error) { // function (options) {
		if jsGlobalValue == nil {
			jsGlobalValue = 1.0
		}
		var bps []any
		if len(o.BlockParamNames()) == 1 {
			v := jsGlobalValue.(float64)
			jsGlobalValue = v + 2
			bps = []any{v, v + 1}
		}
		return fnBP(o, map[string]any{"value": "bar"}, bps)
	},
	"883eaaa19889": func(o *HelperOptions) (any, error) { return o.Lookup("testProperty"), nil }, // function testHelper(options) {

	// subexpressions.json
	"faacc7899f0c": func(o *HelperOptions) (any, error) { return jsAdd(arg(o, 0), arg(o, 0)), nil },       // function (val) { return val + val; }
	"81c58ad34d6a": func(o *HelperOptions) (any, error) { return "LOL", nil },                             // function () { return 'LOL'; }
	"dc5b267caf9e": func(o *HelperOptions) (any, error) { return "val is " + jsStr(arg(o, 0)), nil },      // function (val) { return 'val is ' + val; }
	"5164d1a7abb9": func(o *HelperOptions) (any, error) { return sameContext(arg(o, 0), arg(o, 1)), nil }, // function (x, y) { return x === y; }
	"b1770e78e6be": func(o *HelperOptions) (any, error) { // function (val, that, theOther) { return 'val is ' + val + ', ' + that + ' and ' + theOther; }
		return "val is " + jsStr(arg(o, 0)) + ", " + jsStr(arg(o, 1)) + " and " + jsStr(arg(o, 2)), nil
	},
	"6bb0b8d5fe08": func(o *HelperOptions) (any, error) { return jsStr(arg(o, 0)) + "-" + jsStr(arg(o, 1)), nil }, // function (a, b) { return a + '-' + b; }
	"f2e7444f26ff": func(o *HelperOptions) (any, error) { return jsAdd(arg(o, 0), arg(o, 1)), nil },               // function (a, b) { return a + b; }
	"76f4c611745d": func(o *HelperOptions) (any, error) { // function(x, y, options) { throws if options is reused; return x === y; }
		if o == lastOptions {
			return nil, errors.New("options hash was reused")
		}
		lastOptions = o
		return sameContext(arg(o, 0), arg(o, 1)), nil
	},
	"19739d747f1e": func(o *HelperOptions) (any, error) { return "val is " + jsStr(o.Hash["fun"]), nil }, // function (options) { return "val is " + options.hash.fun; }
	"aac91cb8eab8": func(o *HelperOptions) (any, error) { // function (options) { ... new SafeString('<input aria-label="' + ariaLabel + '" placeholder="' + placeholder + '" />') }
		return SafeString(`<input aria-label="` + escapeString(jsStr(o.Hash["aria-label"])) + `" placeholder="` + escapeString(jsStr(o.Hash["placeholder"])) + `" />`), nil
	},
	"ecc60e33a563": func(o *HelperOptions) (any, error) { return SafeString(jsStr(arg(o, 0))), nil }, // function (defaultString) { return new SafeString(defaultString); }

	// partials.json
	"1ac3ed3f5a1a": func(o *HelperOptions) (any, error) { return "dude", nil },    // function () { return 'dude'; }
	"f1251b711da7": func(o *HelperOptions) (any, error) { return "missing", nil }, // function () { return 'missing'; }
	"5f4a5c794424": func(o *HelperOptions) (any, error) { // function partial(context) { return context.name + ' (' + context.url + ') '; }
		name, _ := lookupProperty(o.Ctx(), "name")
		url, _ := lookupProperty(o.Ctx(), "url")
		return jsStr(name) + " (" + jsStr(url) + ") ", nil
	},

	// string-params.json (and subexpressions.json's string params tests)
	"c63bb0149dad": func(o *HelperOptions) (any, error) { // 'HELP ME MY BOSS ' + passiveVoice + ' ' + noun
		return "HELP ME MY BOSS " + jsStr(arg(o, 0)) + " " + jsStr(arg(o, 1)), nil
	},
	"c0693787c25c": func(o *HelperOptions) (any, error) { // ... + noun + ': ' + options.fn(this)
		s, err := o.Fn()
		return "HELP ME MY BOSS " + jsStr(arg(o, 0)) + " " + jsStr(arg(o, 1)) + ": " + s, err
	},
	"d0db63095849": func(o *HelperOptions) (any, error) { // 'STOP ME FROM READING HACKER NEWS I ' + options.contexts[0][desire] + ' ' + noun
		v, _ := lookupProperty(index(legacy(o).Contexts, 0), jsStr(arg(o, 0)))
		return "STOP ME FROM READING HACKER NEWS I " + jsStr(v) + " " + jsStr(arg(o, 1)), nil
	},
	"d8da07322587": func(o *HelperOptions) (any, error) { // options.fn(options.contexts[0][context])
		v, _ := lookupProperty(index(legacy(o).Contexts, 0), jsStr(arg(o, 0)))
		return o.FnWith(v)
	},
	"fa63a8f5ad15": func(o *HelperOptions) (any, error) { // equal(options.types[0], 'StringLiteral'), ...; return 'Helper called'
		return "Helper called", equal(index(legacy(o).Types, 0), "StringLiteral", index(legacy(o).Types, 1), "PathExpression", index(legacy(o).Types, 2), "BooleanLiteral",
			arg(o, 0), "need", arg(o, 1), "dad.joke", arg(o, 2), true, arg(o, 3), false)
	},
	"680aa5aa9c4f": func(o *HelperOptions) (any, error) { // equal(exclamation, 'he.says'), hashTypes/hash checks; return 'Helper called'
		return "Helper called", equal(arg(o, 0), "he.says", index(legacy(o).Types, 0), "PathExpression",
			legacy(o).HashTypes["desire"], "StringLiteral", legacy(o).HashTypes["noun"], "PathExpression", legacy(o).HashTypes["bool"], "BooleanLiteral",
			o.Hash["desire"], "need", o.Hash["noun"], "dad.joke", o.Hash["bool"], true)
	},
	"f25216650542": func(o *HelperOptions) (any, error) { // ... + options.contexts[0][desire] + ' ' + noun + ' ' + options.fn(this)
		v, _ := lookupProperty(index(legacy(o).Contexts, 0), jsStr(arg(o, 0)))
		s, err := o.Fn()
		return "STOP ME FROM READING HACKER NEWS I " + jsStr(v) + " " + jsStr(arg(o, 1)) + " " + s, err
	},
	"610f8b9e6604": func(o *HelperOptions) (any, error) { return "WITH", nil }, // function () { return 'WITH'; }
	"81831e893336": func(o *HelperOptions) (any, error) { return "VIEW", nil }, // function () { return 'VIEW'; }
	"facda621d588": func(o *HelperOptions) (any, error) { // equal(bar, '@bar'); equal(options.types[0], 'PathExpression'); return 'Foo!'
		return "Foo!", equal(arg(o, 0), "@bar", index(legacy(o).Types, 0), "PathExpression")
	},
	"98bbb426fbe2": func(o *HelperOptions) (any, error) { // equals(a, 'foo'); types are [SubExpression, PathExpression]; return a + b
		return jsAdd(arg(o, 0), arg(o, 1)), equal(arg(o, 0), "foo", len(legacy(o).Types), 2, index(legacy(o).Types, 0), "SubExpression", index(legacy(o).Types, 1), "PathExpression")
	},
	"61a907a40d94": func(o *HelperOptions) (any, error) { // types are [PathExpression]; return a
		return arg(o, 0), equal(len(legacy(o).Types), 1, index(legacy(o).Types, 0), "PathExpression")
	},
	"e101e678717d": func(o *HelperOptions) (any, error) { // equals(options.hashTypes.fun, 'SubExpression'); return 'val is ' + options.hash.fun
		return "val is " + jsStr(o.Hash["fun"]), equal(legacy(o).HashTypes["fun"], "SubExpression")
	},
	"3297100aee77": func(o *HelperOptions) (any, error) { return "BORK", nil }, // function () { return 'BORK'; }

	// track-ids.json. JS null/true ids are "null"/"true" (see LegacyParams.IDs).
	"1ccf18da2b6d": func(o *HelperOptions) (any, error) { // equal(options.ids, undefined); equal(options.hashIds, undefined); return 'success'
		if o.Legacy != nil {
			return nil, errors.New("equal: ids/hashIds set without trackIds")
		}
		return "success", nil
	},
	"14cef3b801c1": func(o *HelperOptions) (any, error) { // ids are [is.a, slave.driver]; 'HELP ME MY BOSS ' + ids[0] + ':' + passiveVoice + ' ' + ids[1] + ':' + noun
		return bossIDs(o), equal(index(legacy(o).IDs, 0), "is.a", index(legacy(o).IDs, 1), "slave.driver")
	},
	"37a6c6d30257": func(o *HelperOptions) (any, error) { // hashIds are {bat: is.a, baz: slave.driver}
		return "HELP ME MY BOSS " + legacy(o).HashIDs["bat"] + ":" + jsStr(o.Hash["bat"]) + " " + legacy(o).HashIDs["baz"] + ":" + jsStr(o.Hash["baz"]),
			equal(legacy(o).HashIDs["bat"], "is.a", legacy(o).HashIDs["baz"], "slave.driver")
	},
	"8dd9d9364650": func(o *HelperOptions) (any, error) { // ids are [is.a, ../slave.driver, is.a, '']
		return bossIDs(o), equal(index(legacy(o).IDs, 0), "is.a", index(legacy(o).IDs, 1), "../slave.driver", index(legacy(o).IDs, 2), "is.a", index(legacy(o).IDs, 3), "")
	},
	"8c14b7f6a8f6": func(o *HelperOptions) (any, error) { // ids are [@is.a, @slave.driver]
		return bossIDs(o), equal(index(legacy(o).IDs, 0), "@is.a", index(legacy(o).IDs, 1), "@slave.driver")
	},
	"f0ab6f71bdcb": func(o *HelperOptions) (any, error) { // ids[0], ids[1], hashIds.key are null
		return "HELP ME MY BOSS " + jsStr(arg(o, 0)) + " " + jsStr(arg(o, 1)) + " " + jsStr(o.Hash["key"]),
			equal(index(legacy(o).IDs, 0), "null", index(legacy(o).IDs, 1), "null", legacy(o).HashIDs["key"], "null")
	},
	"e69260e14a92": func(o *HelperOptions) (any, error) { return 1.0, nil }, // function () { return 1; }
	"b631743b07fe": func(o *HelperOptions) (any, error) { // equal(options.ids[0], true); return 'HELP ME MY BOSS ' + passiveVoice
		return "HELP ME MY BOSS " + jsStr(arg(o, 0)), equal(index(legacy(o).IDs, 0), "true")
	},
	"af10f4358d8e": func(o *HelperOptions) (any, error) { // blockParams = [this.is]; blockParams.path = ['zomg']; options.fn(this, {blockParams})
		var sb strings.Builder
		err := o.s.renderTo(&sb, o.fn, o.Ctx(), bpFrame{vals: []any{o.Lookup("is")}, ids: []string{"zomg"}}, nil)
		return sb.String(), err
	},
	"0654a8ee3da3": func(o *HelperOptions) (any, error) { // ids are [zomg.a, slave.driver, zomg]
		return bossIDs(o), equal(index(legacy(o).IDs, 0), "zomg.a", index(legacy(o).IDs, 1), "slave.driver", index(legacy(o).IDs, 2), "zomg")
	},
	"599e616d20e9": func(o *HelperOptions) (any, error) { // name + ':' + options.ids[0] + '\n'
		return jsStr(arg(o, 0)) + ":" + jsStr(index(legacy(o).IDs, 0)) + "\n", nil
	},
	"e6db14dfaefd": func(o *HelperOptions) (any, error) { // name + ':' + options.data.contextPath + '\n'
		return jsStr(arg(o, 0)) + ":" + jsStr(o.DataVar("contextPath")) + "\n", nil
	},
}

// legacy is o.Legacy, empty when unset.
func legacy(o *HelperOptions) LegacyParams {
	if o.Legacy == nil {
		return LegacyParams{}
	}
	return *o.Legacy
}

// index is JS s[i] (undefined past the end).
func index[T any](s []T, i int) any {
	if i < len(s) {
		return s[i]
	}
	return nil
}

// equal is a run of the spec's equal(got, want) asserts, as got, want pairs.
func equal(pairs ...any) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i] != pairs[i+1] {
			return fmt.Errorf("equal #%d: got %#v, want %#v", i/2, pairs[i], pairs[i+1])
		}
	}
	return nil
}

// bossIDs is 'HELP ME MY BOSS ' + ids[0] + ':' + passiveVoice + ' ' + ids[1] + ':' + noun.
func bossIDs(o *HelperOptions) string {
	return "HELP ME MY BOSS " + jsStr(index(legacy(o).IDs, 0)) + ":" + jsStr(arg(o, 0)) + " " + jsStr(index(legacy(o).IDs, 1)) + ":" + jsStr(arg(o, 1))
}

// jsGlobalValue is the JS global `value` one helper keeps (undefined: nil).
// runRender resets it per test.
var jsGlobalValue any

// arg is JS positional parameter i (undefined when not passed).
func arg(o *HelperOptions, i int) any {
	if i < len(o.Args) {
		return o.Args[i]
	}
	return nil
}

// jsStr is JS String(v) for string concatenation in ported helpers. The
// harness decodes JSON null as nil too, so nil can't tell null from
// undefined; it prints "undefined" (missing args/properties are far more
// common in the corpus than explicit nulls).
func jsStr(v any) string {
	if v == nil {
		return "undefined"
	}
	return string(appendValue(nil, v, false, math.MaxInt))
}

// jsTypeof is JS typeof for the values the harness produces.
func jsTypeof(v any) string {
	switch v.(type) {
	case nil:
		return "undefined"
	case string:
		return "string"
	case float64, int:
		return "number"
	case bool:
		return "boolean"
	case Helper, func(*HelperOptions) (any, error):
		return "function"
	}
	return "object"
}

// toUpperCase is JS v.toUpperCase(): a TypeError unless v is a string.
func toUpperCase(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errors.New("TypeError: toUpperCase of " + jsStr(v))
	}
	return strings.ToUpper(s), nil
}

// fnBP is JS options.fn(ctx, {blockParams: bps}).
func fnBP(o *HelperOptions, ctx any, bps []any) (string, error) {
	var sb strings.Builder
	err := o.FnTo(&sb, ctx, bps, nil)
	return sb.String(), err
}

// equalsBlockParams is the spec's equals(options.fn.blockParams, n).
func equalsBlockParams(o *HelperOptions, n int) error {
	if got := len(o.BlockParamNames()); got != n {
		return errors.New("equals: options.fn.blockParams = " + strconv.Itoa(got) + ", want " + strconv.Itoa(n))
	}
	return nil
}

// fnData is JS options.fn(ctx, {data: createFrame(options.data) + vars}).
func fnData(o *HelperOptions, ctx any, vars map[string]any) (string, error) {
	if vars == nil {
		vars = map[string]any{} // createFrame always adds a frame
	}
	var sb strings.Builder
	err := o.FnTo(&sb, ctx, nil, vars)
	return sb.String(), err
}

// exclaim is (this.exclaim ? s : ”).
func exclaim(o *HelperOptions, s string) string {
	if e, _ := lookupProperty(o.Ctx(), "exclaim"); jsTruthy(e) {
		return s
	}
	return ""
}

// lastOptions is the spec's global.lastOptions.
var lastOptions *HelperOptions

// escapeString is Handlebars.Utils.escapeExpression for a string.
func escapeString(s string) string { return string(appendEscaped(nil, s)) }

// jsAdd is JS a + b for numbers and strings.
func jsAdd(a, b any) any {
	x, xok := a.(float64)
	y, yok := b.(float64)
	if xok && yok {
		return x + y
	}
	return jsStr(a) + jsStr(b)
}

// fnProp is JS options.fn[name]: a property a decorator set on the block.
func fnProp(o *HelperOptions, name string) any {
	if o.fn == nil {
		return nil
	}
	return o.fn.props[name]
}

// equalsArg0 is the spec's equals(options.args[0], want).
func equalsArg0(o *HelperOptions, want any) error {
	if got := arg(o, 0); got != want {
		return errors.New("equals: options.args[0] = " + jsStr(got) + ", want " + jsStr(want))
	}
	return nil
}
