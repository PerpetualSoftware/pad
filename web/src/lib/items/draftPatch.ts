// A kept draft's own changes as anchored patch edits (TASK-3607, PLAN-3578).
// The draft was edited from `base`; when the item has moved on since, restore
// sends only the base→draft changes through the patch endpoint, so a
// collaborator's newer version of the rest of the body is kept.
//
// Each edit is one changed run of lines plus `context` unchanged lines either
// side, quoted verbatim from `base`. The server applies edits in order and
// refuses one whose old text is missing or occurs more than once in the body
// as it stands then (internal/bodypatch). The same check is run here against
// `base`, widening the context until every anchor is unique; if none works
// within MAX_CONTEXT, there is nothing safe to send and restore falls back.

import { diffLines } from 'diff';

export interface DraftEdit {
	old: string;
	new: string;
}

/** Mirrors internal/bodypatch.MaxEdits. */
export const MAX_DRAFT_EDITS = 100;
/** The widest context tried before giving up. */
export const MAX_CONTEXT = 8;

/** Split into lines that keep their terminating newline. */
function lines(text: string): string[] {
	if (text === '') return [];
	const out = text.split(/(?<=\n)/);
	return out;
}

interface Run {
	equal: boolean;
	old: string[];
	new: string[];
}

function runs(base: string, draft: string): Run[] {
	const out: Run[] = [];
	for (const c of diffLines(base, draft)) {
		const ls = lines(c.value);
		const last = out[out.length - 1];
		if (c.added || c.removed) {
			const run = last && !last.equal ? last : { equal: false, old: [], new: [] };
			if (run !== last) out.push(run);
			if (c.added) run.new.push(...ls);
			else run.old.push(...ls);
		} else {
			out.push({ equal: true, old: ls, new: ls });
		}
	}
	return out;
}

function occurrences(body: string, needle: string): number {
	let n = 0;
	for (let at = body.indexOf(needle); at !== -1; at = body.indexOf(needle, at + 1)) n++;
	return n;
}

function editsWithContext(rs: Run[], context: number): DraftEdit[] {
	const edits: DraftEdit[] = [];
	let i = 0;
	while (i < rs.length) {
		if (rs[i].equal) {
			i++;
			continue;
		}
		// Leading context from the equal run before, then changed runs merged
		// while the equal run between them is short enough to be shared context.
		const before = i > 0 ? rs[i - 1].old.slice(-context) : [];
		const old = [...before];
		const neu = [...before];
		let j = i;
		for (;;) {
			old.push(...rs[j].old);
			neu.push(...rs[j].new);
			const gap = rs[j + 1];
			if (gap && gap.equal && rs[j + 2] && gap.old.length <= 2 * context) {
				old.push(...gap.old);
				neu.push(...gap.old);
				j += 2;
				continue;
			}
			if (gap && gap.equal) {
				const after = gap.old.slice(0, context);
				old.push(...after);
				neu.push(...after);
			}
			break;
		}
		edits.push({ old: old.join(''), new: neu.join('') });
		i = j + 1;
	}
	return edits;
}

/** Apply edits as the server would; null if an anchor is missing or ambiguous. */
export function applyDraftEdits(body: string, edits: DraftEdit[]): string | null {
	let cur = body;
	for (const e of edits) {
		if (e.old === '' || occurrences(cur, e.old) !== 1) return null;
		const at = cur.indexOf(e.old);
		cur = cur.slice(0, at) + e.new + cur.slice(at + e.old.length);
	}
	return cur;
}

/**
 * The base→draft changes as anchored edits, or null when they cannot be
 * anchored: an empty base (nothing to quote), more than MAX_DRAFT_EDITS
 * hunks, or an anchor that stays ambiguous at MAX_CONTEXT. Returns [] when
 * the draft equals the base.
 */
export function draftEdits(base: string, draft: string): DraftEdit[] | null {
	if (base === draft) return [];
	if (base === '') return null;
	const rs = runs(base, draft);
	for (let context = 1; context <= MAX_CONTEXT; context++) {
		const edits = editsWithContext(rs, context);
		if (edits.length > MAX_DRAFT_EDITS) continue;
		if (applyDraftEdits(base, edits) === draft) return edits;
	}
	return null;
}

export interface DraftRestorePlan {
	/** The draft's changes anchored on its base, or null if they cannot be. */
	edits: DraftEdit[] | null;
	/**
	 * The same changes anchored on the raw row the draft was kept beside, or
	 * null when no retry is allowed (BUG-3850).
	 */
	rawRetry: DraftEdit[] | null;
}

/**
 * What a restore of a kept draft sends (BUG-3850). A rich draft's base is the
 * editor's snapshot, in the form a flush writes; `rawBaseBody` is the row as
 * stored beside it. The changes anchored on that row are allowed as one retry
 * only when BOTH hold:
 *   - the stored body is still byte-equal to the row, so its anchors are the
 *     body's; and
 *   - the row is the SAME DOCUMENT as the base (`rawIsSameDocument`: the row
 *     in the flush's storage form equals the base), so the two differ only in raw-only
 *     forms (a trailing newline, an autolink's brackets). A row that is ahead
 *     of the editor (it already holds a collaborator's text the editor had not
 *     received when the base was taken) is a different document, and a diff
 *     from it to the draft would delete that text.
 * Like any editor save, the retry also rewrites the raw-only forms in the
 * hunks it sends to the editor's.
 */
export function planDraftRestore(
	d: { markdown: string; baseBody: string; rawBaseBody?: string },
	storedBody: string,
	rawIsSameDocument: boolean,
): DraftRestorePlan {
	const edits = draftEdits(d.baseBody, d.markdown);
	const raw = d.rawBaseBody;
	const rawRetry =
		raw !== undefined && raw !== d.baseBody && storedBody === raw && rawIsSameDocument
			? draftEdits(raw, d.markdown)
			: null;
	return { edits, rawRetry: rawRetry && rawRetry.length > 0 ? rawRetry : null };
}
