import type { ImportCounts, ImportSummary, ImportUniqueDrop } from '$lib/types';

// TASK-3612: say what a bundle import brought over, the bundle's counts next
// to what arrived, so a person moving a workspace can check nothing was lost.

const KINDS: { key: keyof ImportCounts; one: string; many: string }[] = [
	{ key: 'collections', one: 'collection', many: 'collections' },
	{ key: 'items', one: 'item', many: 'items' },
	{ key: 'comments', one: 'comment', many: 'comments' },
	{ key: 'attachments', one: 'attachment', many: 'attachments' }
];

function count(n: number, one: string, many: string): string {
	return `${n} ${n === 1 ? one : many}`;
}

export interface ImportSummaryText {
	/** Every kind arrived in full. */
	complete: boolean;
	text: string;
}

/**
 * The counts as one sentence. Complete: "9 collections, 412 items, 80
 * comments and 37 attachments, everything in the bundle." Short: names each
 * kind whose count differs, as "36 of 37 attachments".
 */
export function describeImportSummary(summary: ImportSummary): ImportSummaryText {
	const short = KINDS.filter((k) => summary.imported[k.key] !== summary.in_bundle[k.key]);
	if (short.length === 0) {
		const parts = KINDS.map((k) => count(summary.imported[k.key], k.one, k.many));
		return {
			complete: true,
			text: `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}, everything in the bundle.`
		};
	}
	const parts = short.map(
		(k) => `${summary.imported[k.key]} of ${count(summary.in_bundle[k.key], k.one, k.many)}`
	);
	return {
		complete: false,
		text: `The counts don't match the bundle: ${parts.join(', ')} arrived. The workspace you exported from is unchanged, so you can delete this copy and import again.`
	};
}

// TASK-3842: the unique values an import dropped (BUG-3825), one line each in
// the CLI's `  dropped: <item>: <key> "<value>" is taken by <holder>` shape.
// The toast renders line breaks, so a server-supplied string must not be
// able to break, reorder or hide the text around it. Every field, the value
// included, has its control, format and line/paragraph separator characters
// (Cc, Cf, Zl, Zp: newlines, U+0085, bidi overrides such as U+202E, U+2028,
// zero-width U+200B) replaced with \uXXXX. Each RAW field is first clipped to
// FIELD_CHARS code points, so a cut can never split an escape; the value is
// then JSON-quoted and escaped after that, because JSON.stringify leaves Cf,
// Zl and Zp as they are.

/** Lines shown before the rest are counted as "and N more". */
export const DROP_LINES_SHOWN = 10;

/** Code points kept of each raw field before it is clipped with "…". */
export const FIELD_CHARS = 120;

const UNSAFE = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu;

function escapeUnsafe(s: string): string {
	// A match outside the BMP is a surrogate pair; each half is escaped.
	return s.replace(UNSAFE, (c) =>
		Array.from({ length: c.length }, (_, i) => `\\u${c.charCodeAt(i).toString(16).padStart(4, '0')}`).join('')
	);
}

function clip(s: string): string {
	const chars = Array.from(s);
	return chars.length > FIELD_CHARS ? chars.slice(0, FIELD_CHARS).join('') + '…' : s;
}

const field = (s: string) => escapeUnsafe(clip(s));

export function importDropLine(d: ImportUniqueDrop): string {
	return `dropped: ${field(d.item)}: ${field(d.key)} ${escapeUnsafe(JSON.stringify(clip(d.value)))} is taken by ${field(d.holder)}`;
}

/**
 * The drops as one message: a lead sentence, then a line per drop, capped at
 * DROP_LINES_SHOWN. Null when nothing was dropped.
 */
export function describeImportDrops(drops: ImportUniqueDrop[] | undefined): string | null {
	if (!drops || drops.length === 0) return null;
	const n = drops.length;
	const lines = drops.slice(0, DROP_LINES_SHOWN).map(importDropLine);
	if (n > DROP_LINES_SHOWN) lines.push(`and ${n - DROP_LINES_SHOWN} more`);
	const lead =
		n === 1
			? 'One unique value was dropped because another item already holds it:'
			: `${n} unique values were dropped because other items already hold them:`;
	return [lead, ...lines].join('\n');
}
