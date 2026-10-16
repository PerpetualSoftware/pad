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

/**
 * The edits a kept draft's restore sends (BUG-3850, lead rulings baa63dc3 and
 * a6380212). None for a stale base: one a remote update or this tab's own send
 * outran, or the row with no confirmed snapshot. The server may hold text the
 * base lacks, and an anchor is a substring, so a patch from it would send that
 * text again and report success ('A' → 'A C U' over a stored 'A C' gives
 * 'A C U C'; 'A' → 'A U1 U2' over 'A U1' gives 'A U1 U2 U1'). The caller
 * shows the conflict notice.
 */
export function restoreEdits(d: { markdown: string; baseBody: string; baseStale?: true }): DraftEdit[] | null {
	return d.baseStale ? null : draftEdits(d.baseBody, d.markdown);
}
