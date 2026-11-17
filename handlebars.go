// Package handlebars is a Handlebars template engine.
package handlebars

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Limits bounds resource usage for untrusted templates. Zero value fields
// fall back to the corresponding DefaultLimits field.
type Limits struct {
	MaxTemplateSize int // bytes of source accepted by Parse
	MaxParseDepth   int // nesting of blocks/subexpressions during parse
	MaxRenderDepth  int // runtime nesting: block scopes + partial expansions
	MaxOutputSize   int // total bytes the renderer may emit
	// MaxOps bounds render work. One op each: substitution, helper or
	// Fn/FnTo call, loop iteration, argument or hash pair, path segment
	// past the first, comment, partial call, inline partial bound or
	// scanned, decorator, and key a hash partial copies; plus one per KiB
	// of a long name or trackIds path the render hashes or copies, of a
	// string whose length it reads, and of equal-length string contexts it
	// compares on entering a block, and one per 8 scope entries a partial
	// block or inline partial copies. So the work per op is bounded.
	MaxOps int
	// MaxExpressions bounds how many expressions Parse accepts: every
	// mustache, block (including each `{{else x}}` link), subexpression,
	// partial, partial block, raw block and decorator counts as one.
	// Parsing stops at the first expression over the limit, before the
	// rest of the source is tokenized. Counting happens while tokenizing,
	// before the parser runs, so an expressions *LimitError is returned
	// even when the source also has an earlier syntax error or a block
	// rejected by NoBlocks (as with MaxTemplateSize).
	MaxExpressions int
}

// DefaultLimits are the values zero Limits fields fall back to, when
// parsing and when rendering. They suit untrusted templates.
var DefaultLimits = Limits{
	MaxTemplateSize: 1 << 20, // 1 MiB
	MaxParseDepth:   100,
	MaxRenderDepth:  100,
	MaxOutputSize:   8 << 20, // 8 MiB
	MaxOps:          1_000_000,
	MaxExpressions:  100_000,
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
	if l.MaxExpressions == 0 {
		l.MaxExpressions = d.MaxExpressions
	}
	return l
}

// LimitError reports which limit was exceeded.
type LimitError struct {
	What string // "template size", "parse depth", "render depth", "output size", "ops", "expressions"
	Max  int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("handlebars: %s limit exceeded (max %d)", e.What, e.Max)
}

// Sentinels for errors.Is, one per limit: a *LimitError matches the one for
// its What, also when wrapped.
var (
	ErrTemplateSize = errors.New("handlebars: template size limit exceeded")
	ErrParseDepth   = errors.New("handlebars: parse depth limit exceeded")
	ErrRenderDepth  = errors.New("handlebars: render depth limit exceeded")
	ErrOutputSize   = errors.New("handlebars: output size limit exceeded")
	ErrOps          = errors.New("handlebars: ops limit exceeded")
	ErrExpressions  = errors.New("handlebars: expressions limit exceeded")
)

// limitSentinels maps LimitError.What to its sentinel.
var limitSentinels = map[string]error{
	"template size": ErrTemplateSize,
	"parse depth":   ErrParseDepth,
	"render depth":  ErrRenderDepth,
	"output size":   ErrOutputSize,
	"ops":           ErrOps,
	"expressions":   ErrExpressions,
}

// Is reports whether target is the sentinel for e.What, e.g. ErrOps for an
// "ops" LimitError.
func (e *LimitError) Is(target error) bool {
	s, ok := limitSentinels[e.What]
	return ok && s == target
}

// Error is a parse or runtime error with template position, like a
// handlebars.js Exception with a loc: Line is 1-based, Column is the 0-based
// JS column (UTF-16 units). Line 0 means no position.
type Error struct {
	Line, Column int
	Message      string
	cause        error // what Unwrap returns: ErrBlocksNotAllowed for a NoBlocks rejection, else nil
}

func (e *Error) Error() string {
	if e.Line > 0 { // mirrors the handlebars.js Exception: "msg - line:col"
		return fmt.Sprintf("%s - %d:%d", e.Message, e.Line, e.Column)
	}
	return e.Message
}

// Unwrap returns ErrBlocksNotAllowed for the error Options.NoBlocks makes
// Parse return, and nil for every other Error.
func (e *Error) Unwrap() error { return e.cause }

// ErrBlocksNotAllowed is what the *Error for a block statement under
// Options.NoBlocks wraps.
var ErrBlocksNotAllowed = errors.New("handlebars: block statements are not allowed")

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
	// StringParams and TrackIDs are the legacy compile options: helpers
	// get the arguments' source strings, types and contexts, or their
	// ids, in HelperOptions.Legacy.
	StringParams bool
	TrackIDs     bool

	// NoBlocks makes Parse reject every block statement, leaving the
	// logic-less subset: substitutions, helper calls, subexpressions,
	// partials, comments and decorators. The error is an *Error at the
	// first block's opening tag that wraps ErrBlocksNotAllowed.
	// RegisterPartial parses with it too; a partial passed to
	// RegisterPartialTemplate keeps its own Options, so parse those with
	// NoBlocks as well.
	NoBlocks bool
}

// Parse parses src with the default Options. See ParseWithOptions.
func Parse(src string) (*Template, error) { return ParseWithOptions(src, Options{}) }

// ParseWithOptions parses src into a Template that renders under opts. A
// source over opts.Limits.MaxTemplateSize, with more than MaxExpressions
// expressions, or nested deeper than MaxParseDepth fails with a
// *LimitError; a syntax error, or what handlebars.js reports when
// compiling, is an *Error, as is a block statement under NoBlocks (it wraps
// ErrBlocksNotAllowed). Parsing takes time linear in len(src).
func ParseWithOptions(src string, opts Options) (*Template, error) {
	prog, err := parse(src, opts)
	if err != nil {
		return nil, err
	}
	return &Template{prog: prog, opts: opts}, nil
}

// Template is a parsed template with its helpers, partials and Logger.
// Rendering is safe for concurrent use; registering helpers or partials and
// setting Logger are not, so do them all before the first render.
type Template struct {
	prog       *program
	opts       Options
	helpers    map[string]Helper
	partials   map[string]partialDef
	decorators map[string]decorator

	// Logger receives {{log}} output: level is "debug", "info", "warn",
	// "error" or "log" (an out-of-range numeric level). Nil discards it.
	// args is the Logger's own copy: it may keep it. It replaces
	// handlebars.js's console-backed logger.log.
	Logger func(level string, args ...any)
}

// RegisterHelper registers h as helper name. A name of a builtin (each,
// if, unless, with, lookup, log) or of the helperMissing and
// blockHelperMissing hooks replaces it. Registration is not
// concurrency-safe; register everything before rendering.
func (t *Template) RegisterHelper(name string, h Helper) {
	if t.helpers == nil {
		t.helpers = map[string]Helper{}
	}
	t.helpers[name] = h
}

// Render renders t with data as the root context and returns the output.
func (t *Template) Render(data any) (string, error) {
	s := newState(t, context.Background(), nil)
	str := ""
	err := s.render(data, nil)
	if err == nil {
		str = string(s.root.buf)
	}
	s.release()
	return str, err
}

// RenderTo renders t with data as the root context into w. It writes to w
// every 4 KiB of output, so when it fails w may already hold part of it.
func (t *Template) RenderTo(w io.Writer, data any) error {
	return t.renderTo(context.Background(), w, data, nil)
}

// RenderCtx checks ctx.Err() every 1024 ops; rendering is otherwise identical.
func (t *Template) RenderCtx(ctx context.Context, w io.Writer, data any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return t.renderTo(ctx, w, data, nil)
}

// RenderWithData is RenderTo plus initial @data values (runtimeOptions.data).
func (t *Template) RenderWithData(w io.Writer, data any, dataVars map[string]any) error {
	return t.renderTo(context.Background(), w, data, dataVars)
}

func (t *Template) renderTo(ctx context.Context, w io.Writer, data any, dataVars map[string]any) error {
	s := newState(t, ctx, w)
	err := s.render(data, dataVars)
	s.release()
	return err
}

// SafeString is written to output without HTML escaping.
type SafeString string

// KeyedCollection lets callers supply insertion-ordered objects (used by
// {{#each}} and property lookup). The spec harness relies on it.
type KeyedCollection interface {
	Keys() []string
	Get(key string) (any, bool)
	Len() int // not called by the library; Keys() gives the length
}
