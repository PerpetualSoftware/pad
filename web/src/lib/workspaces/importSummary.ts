import type { ImportCounts, ImportSummary } from '$lib/types';

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
