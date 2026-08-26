// Parsing for the activity-metadata `changes` string the server emits, e.g.
// "status: open → fixing; priority: low → high". The "; " separator and the
// "→" arrow are produced by diffFields/appendChange in the Go backend
// (internal/server/handlers_documents.go) — keep this in sync if those change.

export interface FieldChange {
	field: string;
	from: string;
	to: string;
}

// Fields whose change pill is suppressed (BUG-2628, Dave's ruling, option 3).
//
// Implementation notes and decision-log entries have had their own timeline
// cards since BUG-2301, so a pill for them can only ever restate what the card
// above it already shows. On items whose notes predate the write-time
// summarizer, it restates it badly: the frozen metadata holds the whole notes
// array as a Go map literal, and the pill renders as a wall of text that
// dwarfs every real change on the same card.
//
// Suppressed at RENDER time, deliberately not at write time. The activity row
// legitimately records THAT notes changed and that belongs in the audit trail;
// what option 3 rules out is showing it as a pill. Dropping it from the record
// would be option 2 — which the filing rejected for destroying the original —
// wearing a different hat.
const SUPPRESSED_CHANGE_FIELDS = new Set(['implementation_notes', 'decision_log']);

// A parsed segment is only a change if its field name is actually a field
// name. The server joins segments with "; " and this splits on ";", so any
// value containing a semicolon fragments — and the fragment's text before its
// first colon then poses as a field name.
//
// This is not belt-and-braces on top of the suppression above; it is
// load-bearing, and measured against the live database. Across the 97 legacy
// rows, suppressing the two field names alone removes 77 of 78 oversized pills
// and leaves the WORST one untouched at 2952 characters — a note's own prose
// contains ";", ":" and "→", so a mid-blob fragment parses as a change whose
// "field" is a paragraph of markdown. With this guard the longest surviving
// pill on those rows is 15 characters.
//
// The test is STRUCTURAL — non-empty, no whitespace, bounded length — rather
// than a lowercase-identifier pattern. The first version of this guard was
// `^[a-z][a-z0-9_]*$` on the reasoning that the server emits schema keys plus
// title/role/assigned, all lowercase. That reasoning was too strong: nothing
// constrains a collection's field keys to lowercase (handlers_collections.go
// compares them with a plain `==`, no case folding), so `Status` or
// `resolution-v2` are legal keys whose pills that pattern would silently drop.
// All 72 field keys in the live database happen to satisfy both forms, which
// is why the measurement below could not tell them apart — a control that
// shows a guard refuses nothing HERE is not evidence that it cannot refuse
// something legitimate.
//
// Measured both ways on the same data: identical on the legacy rows (longest
// surviving pill 15 chars, none over 200) and identical on 6000+ current-format
// rows (6385 pills kept, zero dropped). Same effect, strictly smaller risk.
const MAX_FIELD_KEY_LENGTH = 64;

function looksLikeFieldKey(field: string): boolean {
	return field.length > 0 && field.length <= MAX_FIELD_KEY_LENGTH && !/\s/.test(field);
}

// Split the server's "; "-joined "field: from → to" change string into
// structured entries.
//
// A segment is kept when its value splits into EXACTLY TWO arrow-separated
// parts. Note what that does and does not mean, because the comment here
// previously said the opposite and it is worth being exact: a newly-set field
// arrives as "field: → value", which splits into ["", "value"] — two parts —
// so it IS kept, with an empty `from`. What gets dropped is a segment with no
// arrow at all, or one whose value contains MORE than one arrow (a field whose
// text happens to include "→"). Verified against the function rather than
// inferred from its previous comment.
export function parseFieldChanges(changesStr: string | undefined | null): FieldChange[] {
	if (!changesStr) return [];
	return changesStr
		.split(';')
		.map((part): FieldChange | null => {
			const trimmed = part.trim();
			const colonIdx = trimmed.indexOf(':');
			if (colonIdx === -1) return null;
			const field = trimmed.slice(0, colonIdx).trim();
			if (!looksLikeFieldKey(field)) return null;
			if (SUPPRESSED_CHANGE_FIELDS.has(field)) return null;
			const valuePart = trimmed.slice(colonIdx + 1).trim();
			const arrowParts = valuePart.split('→');
			if (arrowParts.length === 2) {
				return { field, from: arrowParts[0].trim(), to: arrowParts[1].trim() };
			}
			return null;
		})
		.filter((c): c is FieldChange => c !== null);
}

// Sanitize the raw change string for the surfaces that render it as TEXT
// rather than as pills — the dashboard's recent-activity list, and the audit
// page's fallback when nothing parsed.
//
// SCOPE, enumerated rather than sampled (BUG-2628 review round 2). This is a
// DISPLAY rule, so it is applied where a human reads the string and nowhere
// else. The REST endpoints, `--format json`, the bootstrap payload and the
// MCP tools all return the activity metadata verbatim, and that is deliberate
// and not an oversight: the row records THAT notes changed, the ruling was
// about not showing it as a pill, and a client that wants the raw record must
// still be able to get it. Sanitizing the wire would be the option the filing
// rejected for destroying the original.
//
// Two human-facing surfaces are NOT covered here and are filed rather than
// silently included: the CLI (`pad project activity`, `pad workspace
// audit-log`) prints the string with no parsing at all, and the admin console
// audit log renders it verbatim through a generic metadata fallback. Both are
// Go / a different feature, and both need their own decision.
//
// Those surfaces are why suppressing inside parseFieldChanges alone does not
// finish the job, and the audit page is the sharp case: it falls back to the
// raw string when the parsed list is EMPTY, so suppressing every pill on a
// legacy row made the wall of text reappear on the very surface the ruling was
// about. The fix has to be a property of the string, not of the pill list.
//
// Segments are dropped on the same two rules parseFieldChanges applies, and
// every survivor is preserved VERBATIM — including ones parseFieldChanges
// itself will not turn into a pill, such as a value containing more than one
// arrow. That is the point of a fallback, and this keeps it working.
export function formatChangesForDisplay(changesStr: string | undefined | null): string {
	if (!changesStr) return '';
	return changesStr
		.split(';')
		.filter((part) => {
			const trimmed = part.trim();
			const colonIdx = trimmed.indexOf(':');
			if (colonIdx === -1) return false;
			const field = trimmed.slice(0, colonIdx).trim();
			return looksLikeFieldKey(field) && !SUPPRESSED_CHANGE_FIELDS.has(field);
		})
		.map((part) => part.trim())
		.join('; ');
}
