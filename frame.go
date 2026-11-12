package handlebars

// dataFrame is a handlebars.js @data object (utils.js createFrame): a child
// sees its parent's variables unless it sets its own, and `@../x` reads the
// parent (depth walks _parent). Variables are plain fields so an {{#each}}
// reuses one frame for all its iterations. The bools sit together at the
// end so a frame is 72 bytes (size class 80, not 96).
type dataFrame struct {
	parent *dataFrame
	vars   map[string]any // seeded or helper-supplied variables

	key   string // @key, when loop and not arr
	index int    // @index

	// block is @partial-block when blockSet, set by partial calls
	// (runtime.js invokePartial). A set nil block hides the enclosing one.
	block *partialBlock

	// contextPath is JS data.contextPath as TrackIDs builtins and partials
	// set it; "" = unset (JS only ever sets an empty one where the
	// inherited one is falsy too).
	contextPath string

	loop        bool // @key @index @first @last are set (each)
	arr         bool // iterating an array: @key is the index
	first, last bool
	blockSet    bool
}

// newFrame is createFrame(options.data) extended with vars, so an empty
// non-nil vars still adds a frame level (`@../` depth matches JS). It
// returns nil (keep the enclosing frame) when vars is nil or data is off.
func (s *renderState) newFrame(vars map[string]any) *dataFrame {
	if s.data == nil || vars == nil {
		return nil
	}
	return &dataFrame{parent: s.data, vars: vars}
}

// get is data[name], including variables inherited from parent frames.
func (f *dataFrame) get(name string) (any, bool) {
	for ; f != nil; f = f.parent {
		if f.loop {
			switch name {
			case "index":
				return f.index, true
			case "key":
				if f.arr {
					return f.index, true
				}
				return f.key, true
			case "first":
				return f.first, true
			case "last":
				return f.last, true
			}
		}
		if f.contextPath != "" && name == "contextPath" {
			return f.contextPath, true
		}
		if f.blockSet && name == "partial-block" {
			if f.block == nil {
				return nil, true // not a typed nil in the interface
			}
			return f.block, true
		}
		if v, ok := f.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}
