// Package handlebars is a Handlebars template engine.
package handlebars

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var errNotImplemented = errors.New("not implemented")

// Limits bounds resource usage for untrusted templates. Zero value fields
// fall back to the corresponding DefaultLimits field.
type Limits struct {
	MaxTemplateSize int // bytes of source accepted by Parse
	MaxParseDepth   int // nesting of blocks/subexpressions during parse
	MaxRenderDepth  int // runtime nesting: block scopes + partial expansions
	MaxOutputSize   int // total bytes the renderer may emit
	MaxOps          int // substitutions + helper calls + loop iterations
}

var DefaultLimits = Limits{
	MaxTemplateSize: 1 << 20, // 1 MiB
	MaxParseDepth:   100,
	MaxRenderDepth:  100,
	MaxOutputSize:   8 << 20, // 8 MiB
	MaxOps:          1_000_000,
}

// withDefaults replaces zero fields with the corresponding DefaultLimits field.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.MaxTemplateSize == 0 {
		l.MaxTemplateSize = d.MaxTemplateSize
	}
	if l.MaxParseDepth == 0 {
		l.MaxParseDepth = d.MaxParseDepth
	}
	if l.MaxRenderDepth == 0 {
		l.MaxRenderDepth = d.MaxRenderDepth
	}
	if l.MaxOutputSize == 0 {
		l.MaxOutputSize = d.MaxOutputSize
	}
	if l.MaxOps == 0 {
		l.MaxOps = d.MaxOps
	}
	return l
}

// LimitError reports which limit was exceeded.
type LimitError struct {
	What string // "template size", "parse depth", "render depth", "output size", "ops"
	Max  int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("handlebars: %s limit exceeded (max %d)", e.What, e.Max)
}

// Error is a parse or runtime error with template position, like a
// handlebars.js Exception with a loc: Line is 1-based, Column is the 0-based
// JS column (UTF-16 units). Line 0 means no position.
// API change: Column was documented as 1-based; it is the JS 0-based column.
type Error struct {
	Line, Column int
	Message      string
}

func (e *Error) Error() string {
	if e.Line > 0 { // mirrors the handlebars.js Exception: "msg - line:col"
		return fmt.Sprintf("%s - %d:%d", e.Message, e.Line, e.Column)
	}
	return e.Message
}

// Options mirrors handlebars.js compile options plus Limits.
type Options struct {
	Limits                 Limits
	Strict                 bool
	AssumeObjects          bool
	Compat                 bool
	Data                   *bool // nil means true (compileOptions.data)
	KnownHelpers           map[string]bool
	KnownHelpersOnly       bool
	IgnoreStandalone       bool
	PreventIndent          bool
	ExplicitPartialContext bool
	NoEscape               bool
	StringParams           bool // legacy, Phase 11
	TrackIDs               bool // legacy, Phase 11
}

func Parse(src string) (*Template, error) { return ParseWithOptions(src, Options{}) }

func ParseWithOptions(src string, opts Options) (*Template, error) {
	prog, err := parse(src, opts)
	if err != nil {
		return nil, err
	}
	return &Template{prog: prog, opts: opts, bufHint: min(2*len(src), 4096)}, nil
}

// Template is a parsed template.
type Template struct {
	prog    *program
	opts    Options
	helpers map[string]Helper
	bufHint int // Render's initial buffer size
}

// Registration is not concurrency-safe; register everything before rendering.
func (t *Template) RegisterHelper(name string, h Helper) {
	if t.helpers == nil {
		t.helpers = map[string]Helper{}
	}
	t.helpers[name] = h
}

func (t *Template) RegisterPartial(name, src string) error { return errNotImplemented }

func (t *Template) RegisterPartialTemplate(name string, p *Template) {}

func (t *Template) Render(data any) (string, error) {
	out := outWriter{buf: make([]byte, 0, t.bufHint)}
	if err := t.render(context.Background(), &out, data); err != nil {
		return "", err
	}
	return string(out.buf), nil
}

func (t *Template) RenderTo(w io.Writer, data any) error {
	return t.renderTo(context.Background(), w, data)
}

// RenderCtx checks ctx.Err() every 1024 ops; rendering is otherwise identical.
func (t *Template) RenderCtx(ctx context.Context, w io.Writer, data any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return t.renderTo(ctx, w, data)
}

// RenderWithData is RenderTo plus initial @data values (runtimeOptions.data).
func (t *Template) RenderWithData(w io.Writer, data any, dataVars map[string]any) error {
	return t.renderTo(context.Background(), w, data) // Task 7 seeds @data from dataVars
}

func (t *Template) renderTo(ctx context.Context, w io.Writer, data any) error {
	out := outWriter{w: w, buf: make([]byte, 0, flushSize)}
	return t.render(ctx, &out, data)
}

// SafeString is written to output without HTML escaping.
type SafeString string

// Helper implements a handlebars helper.
type Helper func(opts *HelperOptions) (any, error)

// HelperOptions carries evaluated arguments and block closures.
type HelperOptions struct {
	Name string         // helper name as written in the template
	Args []any          // evaluated positional args
	Hash map[string]any // evaluated hash args; nil when the call has none

	s           *renderState
	this        any      // JS `this`
	fn, inverse *program // block programs; nil = none (container.noop)
}

// Ctx is the helper's `this`: the current context, or an empty object when
// that is null/undefined (JS container.nullContext).
func (o *HelperOptions) Ctx() any      { return o.this }
func (o *HelperOptions) IsBlock() bool { return o.fn != nil || o.inverse != nil }

// Fn renders the block with the helper's `this` (JS options.fn(this)).
func (o *HelperOptions) Fn() (string, error) { return o.FnWith(o.this) }

// FnWith renders the block with ctx (JS options.fn(ctx)).
func (o *HelperOptions) FnWith(ctx any) (string, error) { return o.s.renderString(o.fn, ctx) }

func (o *HelperOptions) FnTo(w io.Writer, ctx any, blockParams []any, dataVars map[string]any) error {
	return errNotImplemented
}

// Inverse renders the {{else}} block with the helper's `this`.
func (o *HelperOptions) Inverse() (string, error)             { return o.s.renderString(o.inverse, o.this) }
func (o *HelperOptions) InverseTo(w io.Writer, ctx any) error { return errNotImplemented }
func (o *HelperOptions) DataVar(name string) any              { return nil }
func (o *HelperOptions) Lookup(path string) any               { return nil }
func (o *HelperOptions) BlockParamNames() []string            { return nil }

// KeyedCollection lets callers supply insertion-ordered objects (used by
// {{#each}} and property lookup). The spec harness relies on it.
type KeyedCollection interface {
	Keys() []string
	Get(key string) (any, bool)
	Len() int
}
