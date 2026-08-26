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
// name. The server emits schema keys plus `title`, `role` and `assigned` — all
// lowercase identifiers — so anything else came from splitting a value that
// happened to contain a ";".
//
// This is not belt-and-braces on top of the suppression above; it is
// load-bearing, and measured against the live database. Across the 97 legacy
// rows, suppressing the two field names alone removes 77 of 78 oversized pills
// and leaves the WORST one untouched at 2952 characters — a note's own prose
// contains ";", ":" and "→", so a mid-blob fragment parses as a change whose
// "field" is a paragraph of markdown. With this guard the longest surviving
// pill on those rows is 15 characters.
//
// It cannot refuse a legitimate pill: over 4000 current-format rows carrying
// 4329 pills, it drops zero.
const FIELD_KEY = /^[a-z][a-z0-9_]*$/;

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
			if (!FIELD_KEY.test(field)) return null;
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
