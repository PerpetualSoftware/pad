package textguard

import "strings"

// Repair is the ONE implementation of "make this value satisfy the invariant",
// and it lives beside the predicate for the same reason the predicate is
// shared: four layers that agree about what is REFUSED but disagree about what
// a repair PRODUCES is this bug family arriving one step later.
//
// DOC-2823 S3, on Dave's day-54 ruling: the replacement character is U+FFFD.
// Visible, greppable, and — because the JSON arm emits it as an escape — the
// same six characters wide as the escape it replaces.
//
// isJSON is the caller's classification, exactly as in ParameterRefused, and
// the two must be given the SAME value for the same column. The scan derives it
// from the column list in internal/store/nulcolumns.go, which is Layer B's
// classing.
//
// THE PROPERTY THIS MUST HAVE, in both directions, is what corpus_test.go pins:
//
//   - For every value the layers REFUSE, Repair produces one they ACCEPT.
//   - For every value the layers ACCEPT, Repair is the IDENTITY.
//
// The second half is not politeness. A repair that "tidies" values nobody
// complained about is a repair that rewrites `{"a":"x\\u0000y"}` — six literal
// characters after a doubled backslash, an accepted value — and corrupts it.
// That case is in the corpus precisely because every cheap approach fails it.
func Repair(value string, isJSON bool) string {
	// Raw NULs first, as TEXT. This is byte-preserving everywhere else and is
	// the whole repair for a text-classed column; it is also the right repair
	// for a raw NUL sitting inside a JSON-classed blob, which is a defect of
	// the stored bytes rather than of the document.
	out := strings.ReplaceAll(value, NUL, Replacement)

	// The escape form only matters where something parses the value as a JSON
	// document. In a text column the six characters are six characters, and
	// rewriting them there is the false positive the corpus's third case
	// exists to catch.
	if isJSON && DocumentDecodesNULAnyShape(out) {
		out, _ = RepairJSONEscapes(out)
	}
	return out
}

// RepairJSONEscapes rewrites every NUL escape a JSON parser would DECODE in an
// already-valid JSON document, and reports how many it replaced.
//
// IT IS DELIBERATELY BROADER THAN Repair, and that difference is the whole
// reason it is exported (DOC-2823 S3, the day-54 suspect ruling).
//
// Repair only reaches the scanner for a document DocumentDecodesNULAnyShape
// answers true for — a map-model question, which cannot see a NUL in a value
// shadowed by a LITERAL duplicate key, because the decode keeps the last one.
// This function is a token-level walk over string literals, so it rewrites the
// shadowed escape too. Measured: `Repair` leaves
// `{"a":"<escape>","a":"clean"}` untouched; this returns
// `{"a":"<U+FFFD escape>","a":"clean"}` with a count of 1.
//
// NEVER USE IT AS A PREDICATE. "Would this rewrite something" is not "does a
// layer refuse this", and answering the second question with the first is the
// layer-confusion the whole cluster is made of — textguard.KnownGaps stays a
// recorded shared gap in what is REFUSED. This is only about what a REPAIR,
// asked for explicitly by an operator, is allowed to fix.
func RepairJSONEscapes(s string) (string, int) {
	return repairJSONNULEscapes(s)
}

const (
	// Replacement is U+FFFD as text, for a raw NUL in a stored value.
	Replacement = "�"

	// ReplacementEscape is U+FFFD spelled as a JSON escape, for a live NUL
	// escape inside a JSON document. Six characters replacing six, so the
	// document's byte length is unchanged and nothing around it shifts.
	ReplacementEscape = "\\u" + "fffd"

	// nulEscapeLen is the length of a \uXXXX escape.
	nulEscapeLen = 6
)

// repairJSONNULEscapes rewrites every NUL escape that a JSON parser would
// DECODE, and nothing else.
//
// WHY THIS IS A SCANNER AND NOT decode-walk-remarshal, which was the shape the
// S3 recon write-up proposed before this was written. Re-marshalling a document
// changes four things nobody asked to change: object key order (Go sorts map
// keys), insignificant whitespace, integers wider than float64 (unless the
// decoder is told to use json.Number), and HTML-ish characters (unless the
// encoder is told not to escape them). Worse, a document with LITERAL duplicate
// keys silently loses one on the way through a map — which is one of the two
// gaps BUG-2812 owns, and a repair is the last place that should quietly drop
// user data.
//
// Scanning the raw text has none of those failure modes: every byte the repair
// does not deliberately rewrite is copied verbatim, so a document with no live
// escape comes out byte-identical without that having to be argued.
//
// WHY IT IS NOT A SUBSTRING REPLACE, which is the version that looks equivalent
// and is not. `{"a":"x\\u0000y"}` contains the six characters and decodes to no
// NUL at all, because the doubled backslash makes them literal. Only a scanner
// that consumes escapes IN ORDER can tell the two apart — which is the same
// layer-relativity that made BUG-2803's parity pre-filter unsound, met again in
// the write direction.
//
// The input is a value DocumentDecodesNULAnyShape has already accepted as
// valid JSON, so the scanner has no malformed-input arm: an unterminated string
// or a stray backslash cannot occur. It still copies anything it does not
// recognise rather than assuming, so a future caller passing something else
// gets its bytes back unchanged instead of a mangled document.
func repairJSONNULEscapes(s string) (string, int) {
	var b strings.Builder
	b.Grow(len(s))

	replaced := 0
	inString := false
	for i := 0; i < len(s); {
		c := s[i]

		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			i++
			continue
		}

		switch c {
		case '"':
			inString = false
			b.WriteByte(c)
			i++
		case '\\':
			// An escape. Consume it WHOLE, so its second character can never
			// be read as the start of another one — that is the entire
			// difference between this and a substring replace.
			if i+1 >= len(s) {
				b.WriteByte(c)
				i++
				continue
			}
			if s[i+1] == 'u' && i+nulEscapeLen <= len(s) && isNULEscapeHex(s[i+2:i+nulEscapeLen]) {
				b.WriteString(ReplacementEscape)
				i += nulEscapeLen
				replaced++
				continue
			}
			// Any other escape, including `\\`: copy both bytes and move on.
			b.WriteString(s[i : i+2])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), replaced
}

// isNULEscapeHex reports whether the four hex digits of a \uXXXX escape name
// U+0000.
//
// No case folding, and that is not an oversight: the only spelling of this
// code point is four ASCII zeros, which have no case. Every OTHER code point
// would need it, so a caller widening this function to a general hex compare
// must add it then.
func isNULEscapeHex(hex string) bool {
	if len(hex) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if hex[i] != '0' {
			return false
		}
	}
	return true
}
