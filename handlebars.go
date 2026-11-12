// Package handlebars is a Handlebars template engine.
//
// Task 0: this file is the locked public API with stub bodies.
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

// LimitError reports which limit was exceeded.
type LimitError struct {
	What string // "template size", "parse depth", "render depth", "output size", "ops"
	Max  int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("handlebars: %s limit exceeded (max %d)", e.What, e.Max)
}

// Error is a parse or runtime error with template position (1-based).
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
	return nil, errNotImplemented
}

type Template struct{}

// Registration is not concurrency-safe; register everything before rendering.
func (t *Template) RegisterHelper(name string, h Helper) {}

func (t *Template) RegisterPartial(name, src string) error { return errNotImplemented }

func (t *Template) RegisterPartialTemplate(name string, p *Template) {}

func (t *Template) Render(data any) (string, error) { return "", errNotImplemented }

func (t *Template) RenderTo(w io.Writer, data any) error { return errNotImplemented }

// RenderCtx checks ctx.Err() every 1024 ops; rendering is otherwise identical.
func (t *Template) RenderCtx(ctx context.Context, w io.Writer, data any) error {
	return errNotImplemented
}

// RenderWithData is RenderTo plus initial @data values (runtimeOptions.data).
func (t *Template) RenderWithData(w io.Writer, data any, dataVars map[string]any) error {
	return errNotImplemented
}

// SafeString is written to output without HTML escaping.
type SafeString string

// Helper implements a handlebars helper.
type Helper func(opts *HelperOptions) (any, error)

// HelperOptions carries evaluated arguments and block closures.
type HelperOptions struct {
	Name string         // helper name as written in the template
	Args []any          // evaluated positional args
	Hash map[string]any // evaluated hash args
}

func (o *HelperOptions) Ctx() any                       { return nil }
func (o *HelperOptions) IsBlock() bool                  { return false }
func (o *HelperOptions) Fn() (string, error)            { return "", errNotImplemented }
func (o *HelperOptions) FnWith(ctx any) (string, error) { return "", errNotImplemented }
func (o *HelperOptions) FnTo(w io.Writer, ctx any, blockParams []any, dataVars map[string]any) error {
	return errNotImplemented
}
func (o *HelperOptions) Inverse() (string, error)             { return "", errNotImplemented }
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
