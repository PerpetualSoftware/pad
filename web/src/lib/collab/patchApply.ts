// PLAN-3578 step 2: apply a content-anchored patch to the LIVE editor as a
// narrowed replace, so it merges with typing elsewhere and leaves cursors
// alone. The server hands an open tab the edits (not a body); this module is
// what the designated applier runs.
//
// 1. before = the canonical flushed form of the live document (what an agent
//    reads and quotes). The edits apply to it as TEXT, under the same rules as
//    the server's internal/bodypatch: each old occurs exactly once (overlapping
//    occurrences count), line_hint only chooses among exact matches, in order,
//    all or nothing. That gives after.
// 2. A = parse(before), B = parse(after), each the way setContent parses
//    (detached state, the editor's appendTransaction plugins included; see
//    canonicalMarkdown.ts).
// 3. Live blocks are aligned to A's (LCS) under the ruled rule: equal, or
//    differing only by a link's title attribute or by bold/italic on
//    whitespace (invisible); a trailing empty live paragraph is ignored. A
//    changed A block that aligns to no live block refuses unstable_roundtrip:
//    replacing it would change something the agent never saw.
// 4. The changed top-level blocks (A vs B) replace exactly their aligned live
//    blocks in one transaction. Never a whole-document replace.
import type { Editor } from '@tiptap/core';
import { createDocument } from '@tiptap/core';
import { EditorState, type Plugin } from '@tiptap/pm/state';
import type { Node as PMNode } from '@tiptap/pm/model';

export interface PatchEdit {
	old: string;
	new: string;
	line_hint?: number;
}

export type PatchRefusalReason = 'empty_anchor' | 'anchor_missing' | 'anchor_ambiguous' | 'unstable_roundtrip' | 'parse_refused';

export interface PatchRefusal {
	ok: false;
	reason: PatchRefusalReason;
	edit?: number;
	lines?: number[];
	/** 1-based line in `before` of the first block that could not be aligned. */
	line?: number;
}

export interface PatchApplied {
	ok: true;
	before: string;
	after: string;
}

// ---------------------------------------------------------------- text edits

function matches(s: string, sub: string): number[] {
	const out: number[] = [];
	for (let from = 0; from <= s.length - sub.length; ) {
		const k = s.indexOf(sub, from);
		if (k < 0) break;
		out.push(k);
		from = k + 1;
	}
	return out;
}

function lineOf(s: string, p: number): number {
	let n = 1;
	for (let i = 0; i < p; i++) if (s.charCodeAt(i) === 10) n++;
	return n;
}

/** The server's bodypatch.Apply, in the browser. */
export function applyEditsToText(body: string, edits: PatchEdit[]): { ok: true; after: string } | PatchRefusal {
	for (let i = 0; i < edits.length; i++) if (!edits[i].old) return { ok: false, reason: 'empty_anchor', edit: i };
	for (let i = 0; i < edits.length; i++) {
		const e = edits[i];
		let at = matches(body, e.old);
		if (at.length === 0) return { ok: false, reason: 'anchor_missing', edit: i };
		if (at.length > 1) {
			const hinted = e.line_hint && e.line_hint > 0 ? at.filter((p) => lineOf(body, p) === e.line_hint) : [];
			if (hinted.length !== 1) return { ok: false, reason: 'anchor_ambiguous', edit: i, lines: at.map((p) => lineOf(body, p)) };
			at = hinted;
		}
		body = body.slice(0, at[0]) + e.new + body.slice(at[0] + e.old.length);
	}
	return { ok: true, after: body };
}

// ---------------------------------------------------------------- parsing

const EXCLUDED_PLUGIN_KEY = /^(y-sync|y-undo|yjs-cursor|collaborationCaretAwarenessListener|filterInvalidContent)\$/;

/** setContent's parse, on a detached state (see canonicalMarkdown.ts). Null when a plugin refuses it. */
export function parseDetached(editor: Editor, markdown: string): PMNode | null {
	const storage = (editor.storage as unknown as { markdown: { parser: { parse(s: string): string } } }).markdown;
	const parsed = createDocument(storage.parser.parse(markdown), editor.schema, {}, { errorOnInvalidContent: false });
	const plugins = editor.state.plugins.filter((p: Plugin) => !EXCLUDED_PLUGIN_KEY.test((p as unknown as { key: string }).key));
	const empty = EditorState.create({ schema: editor.schema, plugins });
	const tr = empty.tr.replaceWith(0, empty.doc.content.size, parsed.content);
	const result = empty.applyTransaction(tr);
	if (result.transactions[0] !== tr) return null;
	return result.state.doc;
}

// ---------------------------------------------------------------- alignment

/**
 * The ruled equivalence (PLAN-3578, lead rulings day 91): a live block and a
 * parsed block are the same block when they are equal, or differ only by a
 * link mark's title attribute, or by bold/italic marks on whitespace.
 */
export function blocksEquivalent(live: PMNode, parsed: PMNode): boolean {
	if (live.eq(parsed)) return true;
	return signature(live) === signature(parsed);
}

function signature(n: PMNode): string {
	const out: string[] = [];
	n.descendants((d) => {
		if (d.isText) {
			for (const ch of d.text ?? '') {
				const ws = /\s/.test(ch);
				const marks = d.marks
					.filter((m) => !(ws && (m.type.name === 'bold' || m.type.name === 'italic')))
					.map((m) => (m.type.name === 'link' ? `link:${m.attrs.href}` : `${m.type.name}${JSON.stringify(m.attrs)}`))
					.sort()
					.join(',');
				out.push(ch + '\u0001' + marks);
			}
		} else {
			out.push('\u0002' + d.type.name + JSON.stringify(d.attrs));
		}
		return true;
	});
	return n.type.name + JSON.stringify(n.attrs) + '\u0003' + out.join('\u0004');
}

function isEmptyParagraph(n: PMNode): boolean {
	return n.type.name === 'paragraph' && n.content.size === 0;
}

/** LCS of live (minus a trailing empty paragraph) against a; returns a→live index map. */
function align(live: PMNode, a: PMNode): Map<number, number> {
	let L = live.childCount;
	if (L > 0 && isEmptyParagraph(live.child(L - 1))) L--;
	const M = a.childCount;
	const dp: number[][] = Array.from({ length: L + 1 }, () => new Array(M + 1).fill(0));
	for (let i = L - 1; i >= 0; i--)
		for (let j = M - 1; j >= 0; j--)
			dp[i][j] = blocksEquivalent(live.child(i), a.child(j)) ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
	const map = new Map<number, number>();
	for (let i = 0, j = 0; i < L && j < M; ) {
		if (blocksEquivalent(live.child(i), a.child(j)) && dp[i][j] === dp[i + 1][j + 1] + 1) {
			map.set(j, i);
			i++;
			j++;
		} else if (dp[i + 1][j] >= dp[i][j + 1]) i++;
		else j++;
	}
	return map;
}

function blockStart(doc: PMNode, index: number): number {
	let pos = 0;
	for (let i = 0; i < index; i++) pos += doc.child(i).nodeSize;
	return pos;
}

/** The 1-based line in `md` where A's block `index` starts, best effort (for the refusal). */
function lineOfBlock(a: PMNode, index: number, md: string): number | undefined {
	const text = a.maybeChild(index)?.textContent.trim().split('\n')[0]?.slice(0, 40);
	if (!text) return undefined;
	const at = md.indexOf(text);
	return at < 0 ? undefined : lineOf(md, at);
}

// ---------------------------------------------------------------- apply

export interface PatchContext {
	/** The tab's flush pipeline over a serializer output: the canonical form agents read. */
	toCanonical(rawMarkdown: string): string;
	/** Stored markdown → what setContent is given (toEditorMarkdown). */
	toEditor(stored: string): string;
}

/** Plans the narrowed replace without touching the editor. */
export function planPatch(
	editor: Editor,
	edits: PatchEdit[],
	ctx: PatchContext,
): { ok: true; before: string; after: string; from: number; to: number; slice: PMNode[] } | PatchRefusal {
	const storage = (editor.storage as unknown as { markdown: { serializer: { serialize(d: PMNode): string } } }).markdown;
	const live = editor.state.doc;
	const before = ctx.toCanonical(storage.serializer.serialize(live));
	const text = applyEditsToText(before, edits);
	if (!text.ok) return text;
	const after = text.after;
	const A = parseDetached(editor, ctx.toEditor(before));
	const B = parseDetached(editor, ctx.toEditor(after));
	if (!A || !B) return { ok: false, reason: 'parse_refused' };

	// The changed block range, A vs B: the blocks between their longest
	// common prefix and suffix of equal top-level blocks. Block granularity on
	// purpose; a character-level diff (findDiffStart/End) misplaces a block
	// insertion that repeats its neighbour's text.
	const M = A.childCount;
	const N = B.childCount;
	let pre = 0;
	while (pre < M && pre < N && A.child(pre).eq(B.child(pre))) pre++;
	let suf = 0;
	while (suf < M - pre && suf < N - pre && A.child(M - 1 - suf).eq(B.child(N - 1 - suf))) suf++;
	const aFrom = pre;
	const aTo = M - suf;
	const bFrom = pre;
	const bTo = N - suf;
	if (aFrom === aTo && bFrom === bTo) return { ok: true, before, after, from: 0, to: 0, slice: [] }; // no visible change

	const map = align(live, A);
	let lFrom: number;
	let lTo: number;
	if (aTo > aFrom) {
		const ls: number[] = [];
		for (let j = aFrom; j < aTo; j++) {
			const l = map.get(j);
			if (l === undefined) return { ok: false, reason: 'unstable_roundtrip', line: lineOfBlock(A, j, before) };
			ls.push(l);
		}
		for (let k = 1; k < ls.length; k++)
			if (ls[k] !== ls[k - 1] + 1) return { ok: false, reason: 'unstable_roundtrip', line: lineOfBlock(A, aFrom + k, before) };
		lFrom = ls[0];
		lTo = ls[ls.length - 1] + 1;
	} else {
		// Pure insertion before A[aFrom]: anchor on its neighbours.
		if (aFrom > 0) {
			const prev = map.get(aFrom - 1);
			if (prev === undefined) return { ok: false, reason: 'unstable_roundtrip', line: lineOfBlock(A, aFrom - 1, before) };
			lFrom = lTo = prev + 1;
		} else {
			lFrom = lTo = 0;
		}
	}
	const slice: PMNode[] = [];
	for (let j = bFrom; j < bTo; j++) slice.push(B.child(j));
	return { ok: true, before, after, from: blockStart(live, lFrom), to: blockStart(live, lTo), slice };
}

/** Plans and applies the patch to the live editor in ONE transaction. */
export function applyPatch(editor: Editor, edits: PatchEdit[], ctx: PatchContext): PatchApplied | PatchRefusal {
	const plan = planPatch(editor, edits, ctx);
	if (!plan.ok) return plan;
	if (plan.from !== plan.to || plan.slice.length > 0) {
		const tr = editor.state.tr.replaceWith(plan.from, plan.to, plan.slice);
		editor.view.dispatch(tr);
	}
	return { ok: true, before: plan.before, after: plan.after };
}
