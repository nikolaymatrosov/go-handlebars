package handlebars

// specFuncs maps sha1(JS source)[:12] to the Go Helper (or plain value)
// replacing that JS code object in the spec corpus. A nil value counts as
// missing: the test is SKIPped. Get paste-ready stubs with HBSPEC_STUBS=1.
var specFuncs = map[string]any{}
