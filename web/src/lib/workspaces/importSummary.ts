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
// The toast renders line breaks, so a server-supplied string must not carry
// one: the value is quoted as the CLI's %q quotes it, and a control character
// anywhere else is escaped.

/** Lines shown before the rest are counted as "and N more". */
export const DROP_LINES_SHOWN = 10;

function escapeControls(s: string): string {
	// eslint-disable-next-line no-control-regex
	return s.replace(/[\u0000-\u001f\u007f]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`);
}

export function importDropLine(d: ImportUniqueDrop): string {
	return `dropped: ${escapeControls(d.item)}: ${escapeControls(d.key)} ${JSON.stringify(d.value)} is taken by ${escapeControls(d.holder)}`;
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
