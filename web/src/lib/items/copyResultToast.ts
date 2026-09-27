// The toast a cross-workspace copy or move ends with (BUG-2367, BUG-3230 U1).
//
// Two things can make a copy land differently from a clean success, and the
// copy's own answer says both rather than folding them into a green toast:
//   - a value dropped as NOT UNIQUE (BUG-2367): the preview listed it;
//   - a source body BEHIND its live collaborative document
//     (`warnings.source_content_state`, BUG-3032): an open tab had edits the
//     source row did not hold yet, so the copy carried the older body. The
//     server warns rather than refuses because the source keeps those edits.
//     On a MOVE the source is archived, which does not delete them (archiving is
//     a soft delete and nothing reaps an item's op-log short of a workspace
//     purge), so the remedy names the archived original.
import type { ItemCopyResult } from '$lib/types';

export interface CopyResultToast {
	message: string;
	type: 'success' | 'info';
	/** Undefined leaves the toast store's default. */
	duration?: number;
}

export function copyResultToast(result: ItemCopyResult): CopyResultToast {
	const dest = result.destination;
	const label = dest.ref ?? dest.slug;
	const moved = result.source.archived;
	const verb = moved ? 'Moved' : 'Copied';
	const parts: string[] = [`${verb} to ${dest.workspace_name} as ${label}`];

	const notUnique = result.warnings.not_unique ?? [];
	if (notUnique.length > 0) parts[0] += `, without: ${notUnique.map((d) => d.message).join('; ')}`;

	const stale = !!result.warnings.source_content_state;
	if (stale) {
		parts.push(
			moved
				? 'An open tab had edits to this item that were not saved yet, so the moved copy may be missing them. The archived original still has them: restore it, let it save, then move it again.'
				: 'An open tab had edits to this item that were not saved yet, so the copy may be missing them. Copy it again once they are saved.',
		);
	}

	if (!stale && notUnique.length === 0) return { message: parts[0], type: 'success' };
	return { message: parts.join('. '), type: 'info', duration: stale ? 15000 : 10000 };
}
