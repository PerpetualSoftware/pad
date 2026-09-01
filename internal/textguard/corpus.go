package textguard

// Corpus is the ONE adversarial corpus every layer of the NUL invariant is
// measured against (DOC-2823 S1).
//
// It lives in the shipped package rather than in a _test.go file on purpose:
// the HTTP gate (internal/server), the store's write guard, and the SQLite
// trigger set (S2) are in three different packages, and a corpus duplicated
// per package is three corpora that drift. The four-way differential test
// drives THIS slice through every layer and asserts they agree.
//
// Adding a case here is how a newly-understood shape enters every layer's
// coverage at once. That is the intended way to extend it.
type Case struct {
	// Name is the failure message a reader gets, so it says what the shape IS
	// rather than numbering it.
	Name string

	// Value is the parameter as it would be bound, or the field value as it
	// would arrive in a request body.
	Value string

	// IsJSON is how a layer that classes this value would class it - the
	// column's classification at the store, the key's at the HTTP gate. The
	// differential test asserts the two derivations agree on this corpus.
	IsJSON bool

	// Refused is the answer EVERY layer must give.
	Refused bool

	// Why records the reasoning, and in several cases the measurement, that
	// put the case here. A corpus entry without one is a case nobody can
	// re-derive.
	Why string
}

// NUL is a real NUL byte; EscNUL is the six-character ESCAPE TEXT.
//
// Both are built from Go escape sequences rather than typed literally, and
// that is not fussiness. Typing the escape's six characters by hand produces
// the CHARACTER in several of the places this corpus travels through, and a
// case that means the escape while carrying the character is vacuous. It
// happened three times during BUG-2803 - caught by a surviving mutant, not by
// review - and twice more while writing this file. Anything needing either
// value references these constants, and const_check_test.go asserts they did
// not decay, comparing against a value it BUILDS rather than one it types.
const (
	NUL    = "\x00"
	EscNUL = "\\u0000"
)

// Corpus is deliberately small and adversarial rather than broad. Every entry
// discriminates some pair of layers that could disagree.
var Corpus = []Case{
	{
		Name: "plain text, clean", Value: "ordinary title", IsJSON: false, Refused: false,
		Why: "the control leg. Without it a guard that refuses everything passes every other case.",
	},
	{
		Name: "plain text with a real NUL", Value: "before" + NUL + "after", IsJSON: false, Refused: true,
		Why: "the universal half. Postgres answers SQLSTATE 22021; SQLite stores it and then disagrees with itself about length().",
	},
	{
		Name: "plain text carrying the ESCAPE, not classed as JSON", Value: "before" + EscNUL + "after", IsJSON: false, Refused: false,
		Why: "six ordinary characters in a text column. Refusing this is the false positive that made the raw-byte parity pre-filter unsound.",
	},
	{
		Name: "JSON document whose VALUE decodes to a NUL", Value: `{"a":"x` + EscNUL + `y"}`, IsJSON: true, Refused: true,
		Why: "the 22P05 shape. Postgres's own jsonb parser refuses it; the outer string is pure ASCII so no text-encoding check sees it.",
	},
	{
		Name: "JSON document whose KEY decodes to a NUL", Value: `{"k` + EscNUL + `ey":"v"}`, IsJSON: true, Refused: true,
		Why: "keys are as fatal as values and are the half a value-only walk misses. json_tree exposes them in the key column (TASK-2824).",
	},
	{
		Name: "JSON document with a DOUBLED backslash", Value: `{"a":"x\` + EscNUL + `y"}`, IsJSON: true, Refused: false,
		Why: "literal text after decoding, not a NUL. This is the exact trap the parity pre-filter failed: measured on modernc, it stays 8 characters with no NUL (TASK-2824).",
	},
	{
		Name: "JSON-classed value carrying a RAW NUL", Value: `{"a":"x` + NUL + `y"}`, IsJSON: true, Refused: true,
		Why: "codex round 9's regression on BUG-2803: taking the JSON branch used to SKIP the plain check, so a direct NUL in a fields blob was accepted. Both checks always.",
	},
	{
		Name: "JSON-classed value that is not a document", Value: "just a string", IsJSON: true, Refused: false,
		Why: "classing says the column holds JSON; this value does not parse as one. The guard must not refuse it on classing alone.",
	},
	{
		Name: "nested document one layer deeper", Value: `{"a":"{\"b\":\"x\` + EscNUL + `y\"}"}`, IsJSON: true, Refused: false,
		Why: "the escape is inside a string the blob merely CARRIES, not in the layer Postgres parses. Fatal one level up, stored intact here - the asymmetry is the point.",
	},
	{
		Name: "empty string", Value: "", IsJSON: false, Refused: false,
		Why: "boundary. IsJSONDocument indexes t[0] and must not panic on it.",
	},
	{
		Name: "empty string classed as JSON", Value: "", IsJSON: true, Refused: false,
		Why: "same boundary through the other arm.",
	},
	{
		Name: "JSON array whose element decodes to a NUL", Value: `["ok","x` + EscNUL + `y"]`, IsJSON: true, Refused: true,
		Why: "arrays are a document shape too - items.tags is a JSON array column, so the array walk is load-bearing, not symmetry.",
	},
	{
		Name: "value that is only the escape", Value: EscNUL, IsJSON: false, Refused: false,
		Why: "six characters, no document, no NUL. The MCP audit path found that a value made entirely of escapes is non-empty as text and empty as decoded.",
	},
	{
		Name: "NUL at the very end", Value: "trailing" + NUL, IsJSON: false, Refused: true,
		Why: "position independence. A C-string-minded check that stops at the terminator reads this as clean, and SQLite's own length() does exactly that.",
	},
	{
		Name: "NUL at the very start", Value: NUL + "leading", IsJSON: false, Refused: true,
		Why: "the other end, for the same reason.",
	},
}
