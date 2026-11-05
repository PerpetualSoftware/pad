// Chat message ref tokenising and resolution (TASK-4044).
//
// extractRefs is a port of internal/links/extract.go::ExtractWikiLinks,
// narrowed to the two kinds the chat enrichment looks at (`[[REF]]` and
// `[[workspace::REF]]`). It must find exactly the links the server found, or a
// body would render a link the page-level `refs` list has no entry for. The
// code-span and fence scanners are line-for-line ports of fencedCodeRanges,
// findFenceCloser and inlineCodeRanges there; the web renderer's own
// markdown.ts::codeRanges is a deliberately simpler variant and is NOT used.

import type { ChatMessage, ChatRef } from '$lib/types';
import { trimSpace } from './presence';

// Same production as links.wikiLinkPattern / markdown.ts::WIKI_LINK_PATTERN_SOURCE.
const WIKI_LINK = String.raw`\[\[((?:\\[^\n]|[^\]\\])+)\]\]`;
const REF_PATTERN = /^[A-Za-z][A-Za-z0-9]*-\d+$/;
const WORKSPACE_SLUG = /^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$/;

type Range = [number, number];

function nextLine(content: string, i: number): number {
	const nl = content.indexOf('\n', i);
	return nl < 0 ? -1 : nl + 1;
}

function findFenceCloser(content: string, start: number, tickCount: number, fenceChar: string): number {
	const n = content.length;
	let i = start;
	while (i < n) {
		const nl = content.indexOf('\n', i);
		if (nl < 0) return -1;
		const lineStart = nl + 1;
		if (lineStart >= n) return -1;
		let closerStart = lineStart;
		let spaces = 0;
		while (spaces < 4 && closerStart < n && content[closerStart] === ' ') {
			closerStart++;
			spaces++;
		}
		if (spaces >= 4) {
			i = lineStart;
			continue;
		}
		let j = closerStart;
		while (j < n && content[j] === fenceChar) j++;
		if (j - closerStart >= tickCount) {
			let rest = j;
			while (rest < n && content[rest] !== '\n') {
				if (content[rest] !== ' ') break;
				rest++;
			}
			if (rest >= n || content[rest] === '\n') return closerStart;
		}
		i = lineStart;
	}
	return -1;
}

function fencedCodeRanges(content: string): Range[] {
	const ranges: Range[] = [];
	const n = content.length;
	let i = 0;
	while (i < n) {
		if (i > 0 && content[i - 1] !== '\n') {
			const nx = nextLine(content, i);
			if (nx < 0) return ranges;
			i = nx;
			continue;
		}
		let fenceLineStart = i;
		let spaces = 0;
		while (spaces < 4 && fenceLineStart < n && content[fenceLineStart] === ' ') {
			fenceLineStart++;
			spaces++;
		}
		if (spaces >= 4) {
			const nx = nextLine(content, i);
			if (nx < 0) return ranges;
			i = nx;
			continue;
		}
		const ch = fenceLineStart < n ? content[fenceLineStart] : '';
		if (ch !== '`' && ch !== '~') {
			const nx = nextLine(content, i);
			if (nx < 0) return ranges;
			i = nx;
			continue;
		}
		const tickStart = fenceLineStart;
		let j = fenceLineStart;
		while (j < n && content[j] === ch) j++;
		const tickCount = j - tickStart;
		if (tickCount < 3) {
			const nx = nextLine(content, i);
			if (nx < 0) return ranges;
			i = nx;
			continue;
		}
		if (ch === '`') {
			const restEnd = content.indexOf('\n', j);
			const restLimit = restEnd < 0 ? n : restEnd;
			const tick = content.indexOf('`', j);
			if (tick >= 0 && tick < restLimit) {
				const nx = nextLine(content, i);
				if (nx < 0) return ranges;
				i = nx;
				continue;
			}
		}
		i = j;
		const closer = findFenceCloser(content, i, tickCount, ch);
		if (closer < 0) {
			ranges.push([tickStart, n]);
			return ranges;
		}
		let k = closer;
		while (k < n && content[k] === ch) k++;
		ranges.push([tickStart, k]);
		i = k;
	}
	return ranges;
}

function isBlankLineAt(content: string, pos: number): boolean {
	for (let i = pos; i < content.length; i++) {
		const c = content[i];
		if (c === '\n') return true;
		if (c !== ' ' && c !== '\t') return false;
	}
	return true;
}

function inlineCodeRanges(content: string, fenced: Range[]): Range[] {
	const ranges: Range[] = [];
	const n = content.length;
	let i = 0;
	let fi = 0;
	while (i < n) {
		while (fi < fenced.length && fenced[fi][1] <= i) fi++;
		if (fi < fenced.length && fenced[fi][0] <= i) {
			i = fenced[fi][1];
			fi++;
			continue;
		}
		const b = content.indexOf('`', i);
		if (b < 0) return ranges;
		const openStart = b;
		if (fi < fenced.length && fenced[fi][0] <= openStart && openStart < fenced[fi][1]) {
			i = fenced[fi][1];
			fi++;
			continue;
		}
		let j = openStart + 1;
		while (j < n && content[j] === '`') j++;
		const openLen = j - openStart;
		let closerEnd = -1;
		let k = j;
		while (k < n) {
			if (content[k] === '\n') {
				if (isBlankLineAt(content, k + 1)) break;
				k++;
				continue;
			}
			if (content[k] !== '`') {
				k++;
				continue;
			}
			const runStart = k;
			while (k < n && content[k] === '`') k++;
			if (k - runStart === openLen) {
				closerEnd = k;
				break;
			}
		}
		if (closerEnd < 0) {
			i = openStart + 1;
			continue;
		}
		ranges.push([openStart, closerEnd]);
		i = closerEnd;
	}
	return ranges;
}

function inRanges(ranges: Range[], pos: number): boolean {
	for (const [s, e] of ranges) if (pos >= s && pos < e) return true;
	return false;
}

function unescapeWikiBody(s: string): string {
	return s.replace(/\\([\\\]|])/g, '$1');
}

function splitOnUnescapedPipe(body: string): { key: string; suffix: string } | null {
	let i = 0;
	while (i < body.length) {
		if (body[i] === '\\' && i + 1 < body.length) {
			i += 2;
			continue;
		}
		if (body[i] === '|') return { key: body.slice(0, i), suffix: body.slice(i + 1) };
		i++;
	}
	return null;
}

/** links.CanonicalizeRef: upper-case everything before the last hyphen. */
export function canonicalizeRef(ref: string): string {
	const dash = ref.lastIndexOf('-');
	if (dash < 0) return ref.toUpperCase();
	return ref.slice(0, dash).toUpperCase() + ref.slice(dash);
}

export interface ExtractedRef {
	/** The literal `[[...]]` text, for rendering a link that does not resolve. */
	raw: string;
	start: number;
	end: number;
	/** Set only for the `[[workspace::REF]]` form. */
	workspace?: string;
	/** Canonical (upper-cased prefix) ref. */
	ref: string;
	/** The `|Display` override, when present. */
	display?: string;
}

/** The ref and workspace-ref links in `content`, outside code, in source order. */
export function extractRefs(content: string): ExtractedRef[] {
	if (!content) return [];
	const fenced = fencedCodeRanges(content);
	const inline = inlineCodeRanges(content, fenced);
	const out: ExtractedRef[] = [];
	const re = new RegExp(WIKI_LINK, 'g');
	let m: RegExpExecArray | null;
	while ((m = re.exec(content)) !== null) {
		const start = m.index;
		if (inRanges(fenced, start) || inRanges(inline, start)) continue;
		let body = m[1];
		let display: string | undefined;
		const split = splitOnUnescapedPipe(body);
		if (split) {
			display = unescapeWikiBody(split.suffix);
			body = split.key;
		}
		const unescaped = unescapeWikiBody(body);
		if (unescaped === '') continue;
		const trimmed = trimSpace(unescaped);
		const raw = m[0];
		const end = start + raw.length;
		const sep = trimmed.indexOf('::');
		if (sep >= 0) {
			const ws = trimSpace(trimmed.slice(0, sep));
			const rest = trimSpace(trimmed.slice(sep + 2));
			if (WORKSPACE_SLUG.test(ws) && REF_PATTERN.test(rest)) {
				out.push({ raw, start, end, workspace: ws, ref: canonicalizeRef(rest), display });
				continue;
			}
		}
		if (REF_PATTERN.test(trimmed)) {
			out.push({ raw, start, end, ref: canonicalizeRef(trimmed), display });
		}
	}
	return out;
}

export type RefSegment =
	| { type: 'text'; text: string }
	| { type: 'link'; text: string; href: string; title: string; workspace: string; ref: string }
	| { type: 'restricted'; text: 'restricted item'; workspace: string; ref: string };

/** The (workspace, canonical ref) key a verdict is held under (3c D4: exported). */
export function refKey(workspace: string, ref: string): string {
	return `${workspace}\u0000${ref}`;
}

/**
 * Split a message body into text, link and restricted segments.
 *
 * A ref's workspace is its explicit one, else the message's context workspace
 * slug. It is looked up in the page-level `refs` by (workspace, canonical ref):
 * `visible` becomes a link to `/-/r/{workspace}/{REF}` (which re-checks access
 * on click), `restricted` a title-less placeholder, and anything else (an
 * `unresolved` entry, no entry because the server dropped it, or a bare ref
 * with no context workspace) stays plain text exactly as written.
 */
export function tokenizeBody(
	msg: Pick<ChatMessage, 'body' | 'context_workspace' | 'deleted'>,
	refs: readonly ChatRef[]
): RefSegment[] {
	if (msg.deleted || !msg.body) return msg.body ? [{ type: 'text', text: msg.body }] : [];
	const byKey = new Map<string, ChatRef>();
	for (const r of refs) byKey.set(refKey(r.workspace, r.ref), r);
	const segs: RefSegment[] = [];
	let pos = 0;
	const pushText = (text: string) => {
		if (!text) return;
		const last = segs[segs.length - 1];
		if (last && last.type === 'text') last.text += text;
		else segs.push({ type: 'text', text });
	};
	for (const e of extractRefs(msg.body)) {
		pushText(msg.body.slice(pos, e.start));
		pos = e.end;
		const workspace = e.workspace ?? msg.context_workspace?.slug ?? '';
		const hit = workspace ? byKey.get(refKey(workspace, e.ref)) : undefined;
		if (hit?.state === 'visible') {
			segs.push({
				type: 'link',
				text: e.display ?? e.ref,
				href: `/-/r/${workspace}/${e.ref}`,
				title: hit.title ?? '',
				workspace,
				ref: e.ref
			});
		} else if (hit?.state === 'restricted') {
			segs.push({ type: 'restricted', text: 'restricted item', workspace, ref: e.ref });
		} else {
			pushText(e.raw);
		}
	}
	pushText(msg.body.slice(pos));
	return segs;
}

const escapeText = (s: string): string =>
	s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');

export interface RefSwap {
	/** Plain alphanumeric text standing in for the ref while markdown runs. */
	token: string;
	/** The HTML to put where the token landed; untrusted until sanitised. */
	html: string;
	/** The ref as written, for a token that ends up somewhere HTML cannot go. */
	literal: string;
}

/**
 * Markdown-renderer counterpart of tokenizeBody (TASK-4315): the same verdicts
 * for the same refs (extractRefs, so code spans and fences are excluded and the
 * Go extractor is matched), but expressed as tokens in the text plus the HTML
 * each token becomes. The caller runs markdown over `text`, swaps the tokens
 * into the result, and sanitises the whole once. `visible` becomes a
 * /-/r/{workspace}/{REF} link, `restricted` a title-less placeholder, and
 * anything else (unresolved, dropped by the server, a bare ref with no context
 * workspace) is left in `text` exactly as written. Titles and display text are
 * untrusted: they are escaped here and the sanitiser has the last word.
 */
export function replaceRefs(
	body: string,
	refs: readonly ChatRef[],
	ctxWorkspace: string | undefined
): { text: string; swaps: RefSwap[] } {
	const found = body ? extractRefs(body) : [];
	if (found.length === 0) return { text: body, swaps: [] };
	const byKey = new Map<string, ChatRef>();
	for (const r of refs) byKey.set(refKey(r.workspace, r.ref), r);
	// A prefix the body does not contain, so no token can collide with authored text.
	let n = 0;
	while (body.includes(`zqpadref${n}x`)) n++;
	const prefix = `zqpadref${n}x`;
	const swaps: RefSwap[] = [];
	let text = '';
	let pos = 0;
	for (const e of found) {
		const workspace = e.workspace ?? ctxWorkspace ?? '';
		const hit = workspace ? byKey.get(refKey(workspace, e.ref)) : undefined;
		let html: string | null = null;
		if (hit?.state === 'visible') {
			const title = hit.title ? ` title="${escapeText(hit.title)}"` : '';
			html = `<a href="/-/r/${workspace}/${e.ref}"${title}>${escapeText(e.display ?? e.ref)}</a>`;
		} else if (hit?.state === 'restricted') {
			html = '<span class="ref-restricted">restricted item</span>';
		}
		if (html === null) continue;
		const token = `${prefix}${swaps.length}z`;
		swaps.push({ token, html, literal: e.raw });
		text += body.slice(pos, e.start) + token;
		pos = e.end;
	}
	return { text: text + body.slice(pos), swaps };
}
