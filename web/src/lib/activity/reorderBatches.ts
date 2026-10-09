// Collapsing a reorder's activity rows for display (TASK-3517).
//
// PUT /items/sort-order writes ONE "reordered" row per moved item, each with
// the same `reorder_batch` id in its metadata. Per-item rows keep the feed's
// per-row visibility filter correct: a viewer receives only the rows for items
// they can see. A twenty-card drag would still be twenty lines, so the feed
// collapses each run of consecutive rows from one batch into a single line
// AFTER the server's filter. The count is what this viewer can see.

import type { Activity } from '$lib/types';

export type CollapsedActivity = Activity & {
	/** Rows in this reorder batch the viewer can see; set on a collapsed line. */
	reorder_count?: number;
};

function batchOf(a: Activity): string {
	if (a.action !== 'reordered' || !a.metadata) return '';
	try {
		const meta = JSON.parse(a.metadata) as Record<string, unknown>;
		return typeof meta.reorder_batch === 'string' ? meta.reorder_batch : '';
	} catch {
		return '';
	}
}

/**
 * Collapse each run of consecutive "reordered" rows sharing a reorder_batch
 * into its first row, carrying the run's length as `reorder_count`. A run of
 * one stays a plain row. Rows are never reordered, and anything that isn't a
 * reorder passes through unchanged.
 */
export function collapseReorderBatches(rows: readonly Activity[]): CollapsedActivity[] {
	const out: CollapsedActivity[] = [];
	let runBatch = '';
	for (const row of rows) {
		const batch = batchOf(row);
		const last = out[out.length - 1];
		if (batch && batch === runBatch && last) {
			last.reorder_count = (last.reorder_count ?? 1) + 1;
			continue;
		}
		runBatch = batch;
		out.push({ ...row });
	}
	return out;
}
