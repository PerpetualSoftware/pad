// Parsing for the activity-metadata `changes` string the server emits, e.g.
// "status: open → fixing; priority: low → high". The "; " separator and the
// "→" arrow are produced by diffFields/appendChange in the Go backend
// (internal/server/handlers_documents.go) — keep this in sync if those change.

export interface FieldChange {
	field: string;
	from: string;
	to: string;
}

// Split the server's "; "-joined "field: from → to" change string into
// structured entries. Segments that don't carry a "from → to" transition
// (e.g. a newly-set field rendered as "field: → value") are dropped so the
// caller can render clean two-sided pills; use the raw string for those.
export function parseFieldChanges(changesStr: string | undefined | null): FieldChange[] {
	if (!changesStr) return [];
	return changesStr
		.split(';')
		.map((part): FieldChange | null => {
			const trimmed = part.trim();
			const colonIdx = trimmed.indexOf(':');
			if (colonIdx === -1) return null;
			const field = trimmed.slice(0, colonIdx).trim();
			const valuePart = trimmed.slice(colonIdx + 1).trim();
			const arrowParts = valuePart.split('→');
			if (arrowParts.length === 2) {
				return { field, from: arrowParts[0].trim(), to: arrowParts[1].trim() };
			}
			return null;
		})
		.filter((c): c is FieldChange => c !== null);
}
