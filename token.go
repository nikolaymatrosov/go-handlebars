package handlebars

type tokenKind uint8

const (
	tEOF tokenKind = iota
	tBOOLEAN
	tCLOSE
	tCLOSE_BLOCK_PARAMS
	tCLOSE_RAW_BLOCK
	tCLOSE_SEXPR
	tCLOSE_UNESCAPED
	tCOMMENT
	tCONTENT
	tDATA
	tEND_RAW_BLOCK
	tEQUALS
	tID
	tINVALID
	tINVERSE
	tNULL
	tNUMBER
	tOPEN
	tOPEN_BLOCK
	tOPEN_BLOCK_PARAMS
	tOPEN_ENDBLOCK
	tOPEN_INVERSE
	tOPEN_INVERSE_CHAIN
	tOPEN_PARTIAL
	tOPEN_PARTIAL_BLOCK
	tOPEN_RAW_BLOCK
	tOPEN_SEXPR
	tOPEN_UNESCAPED
	tSEP
	tSTRING
	tUNDEFINED
)

var tokenNames = [...]string{
	"EOF", "BOOLEAN", "CLOSE", "CLOSE_BLOCK_PARAMS", "CLOSE_RAW_BLOCK", "CLOSE_SEXPR",
	"CLOSE_UNESCAPED", "COMMENT", "CONTENT", "DATA", "END_RAW_BLOCK", "EQUALS", "ID",
	"INVALID", "INVERSE", "NULL", "NUMBER", "OPEN", "OPEN_BLOCK", "OPEN_BLOCK_PARAMS",
	"OPEN_ENDBLOCK", "OPEN_INVERSE", "OPEN_INVERSE_CHAIN", "OPEN_PARTIAL",
	"OPEN_PARTIAL_BLOCK", "OPEN_RAW_BLOCK", "OPEN_SEXPR", "OPEN_UNESCAPED", "SEP",
	"STRING", "UNDEFINED",
}

// String returns the handlebars.js token name.
func (k tokenKind) String() string { return tokenNames[k] }

// token is one lexeme. text is a subslice of the source except where the
// grammar transforms it (escaped mustaches, string/ID unescaping, raw end tags).
// line is 1-based; col is 0-based in UTF-16 units (JS yylloc.first_column);
// off is the byte offset of the token start in the source.
type token struct {
	kind      tokenKind
	text      string
	line, col int
	off       int
}
