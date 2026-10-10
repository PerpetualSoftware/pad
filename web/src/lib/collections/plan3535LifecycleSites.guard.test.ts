// PLAN-3535's classification guard, the web half (the Go half is
// internal/models/plan3535_lifecycle_sites_guard_test.go).
//
// Every call of a lifecycle helper, the functions that decide whether an item
// is open, done or counted, is listed below with a classification:
// "exclude: <how reference items are left out>" or "keep: <why this site
// counts every item>". A new call site, a site that stops calling, or a new
// hardcoded done list fails until someone decides which it is. That is the
// point: a counting site written later cannot silently count a doc under a
// plan as unfinished work.
//
// DELIBERATELY NAME-BASED (file + helper + number of calls), not an analysis
// of what a site counts: the reason is what review argues about, and a scanner
// that tried to judge it would invite an open-ended review loop. What proves
// the counts themselves is childProgress.test.ts and the server's behavioural
// fixture (TestPLAN3535_ReferenceItemsCountTowardNoOpenWork).
import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

const HELPERS = [
	'childState',
	'childRowState',
	'statusState',
	'countChildProgress',
	'countedChildren',
	'isChildDone',
	'isReferenceChild',
	'splitReferenceChildren',
	'getTerminalOptions',
	'doneFieldTerminalOptions',
	'getAbandonedOptions',
	'isTerminalStatus',
	'isTerminalStatusDefault'
];

// "path::helper" → [number of calls, classification].
const SITES: Record<string, [number, string]> = {
	// The hub, composed of itself and of the per-collection resolvers.
	'src/lib/collections/childProgress.ts::childState': [6, 'exclude: the hub; every counting door here asks isReferenceChild first'],
	'src/lib/collections/childProgress.ts::isReferenceChild': [3, 'exclude: the reference predicate, asked by split / counted / count'],
	'src/lib/collections/childProgress.ts::doneFieldTerminalOptions': [1, 'exclude: the hub reads the done field'],
	'src/lib/collections/childProgress.ts::getAbandonedOptions': [1, 'exclude: the hub reads the abandoned values'],
	'src/lib/collections/childProgress.ts::isTerminalStatusDefault': [1, 'exclude: the hub falls back to the default list'],
	'src/lib/types/index.ts::doneFieldTerminalOptions': [1, 'keep: the resolver itself'],
	'src/lib/types/index.ts::getTerminalOptions': [1, 'keep: the resolver itself'],

	// Parent progress and the parent page.
	'src/lib/components/ChildChart.svelte::countedChildren': [1, 'exclude: countedChildren skips reference children (and ChildItems passes work children only)'],
	'src/lib/components/ChildChart.svelte::isChildDone': [1, 'exclude: called on counted children only'],
	'src/lib/components/ChildItems.svelte::splitReferenceChildren': [1, 'exclude: reference children render in their own References group, outside progress'],
	'src/lib/components/ChildItems.svelte::childRowState': [2, 'exclude: per-row done styling, on work children only (screen and print)'],
	'src/lib/components/NestedChildren.svelte::countChildProgress': [1, 'exclude: countChildProgress skips reference children'],
	'src/lib/components/NestedChildren.svelte::childRowState': [1, 'exclude: per-row done styling, gated by isReferenceChild'],
	'src/lib/components/NestedChildren.svelte::isReferenceChild': [1, 'exclude: a reference row gets no done styling'],

	// Dashboard collection cards.
	'src/routes/[username]/[workspace]/+page.svelte::statusState': [1, 'exclude: collProgress draws no bar for a reference collection; the count keeps every item'],

	// Keep.
	'src/lib/components/items/ItemDetail.svelte::getTerminalOptions': [1, "keep: feeds ChildItems' terminalStatuses, which nothing reads since PLAN-3535 (removal is a follow-up: ItemDetail's identity gate re-flags every reviewed unit on an import change)"],
	'src/lib/components/collections/BoardView.svelte::doneFieldTerminalOptions': [1, "keep: the board's terminal-lane display cap, a collection's own view"],
	'src/lib/items/localLists.ts::childState': [1, "keep: tags and starred pages filter an item's own open state; a reference doc can be open"]
};

// A literal done list: an array opening with 'done' followed by another
// terminal word. That is how the web drifted from the server before (three
// components carried their own). Only the canonical default may hold one.
const DONE_LIST = /\[\s*(['"])done\1\s*,\s*(['"])(completed|cancelled|resolved|fixed)\2/g;
const DONE_LISTS: Record<string, number> = {
	'src/lib/types/index.ts': 1, // DEFAULT_TERMINAL_STATUSES, the canonical default
	'src/lib/components/items/ItemDetail.svelte': 1 // the dead terminalStatuses fallback above
};

const webRoot = resolve(__dirname, '../../..');

function sourceFiles(dir: string): string[] {
	const out: string[] = [];
	for (const name of readdirSync(dir)) {
		const path = join(dir, name);
		if (statSync(path).isDirectory()) {
			out.push(...sourceFiles(path));
		} else if (/\.(ts|svelte)$/.test(name) && !/\.test\.ts$/.test(name)) {
			out.push(path);
		}
	}
	return out;
}

function stripComments(src: string): string {
	return src
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.replace(/<!--[\s\S]*?-->/g, '')
		.replace(/^[ \t]*\/\/.*$/gm, '');
}

describe('PLAN-3535 lifecycle call sites', () => {
	const found: Record<string, number> = {};
	const doneLists: Record<string, number> = {};
	for (const file of sourceFiles(join(webRoot, 'src'))) {
		const rel = relative(webRoot, file).split('\\').join('/');
		const src = stripComments(readFileSync(file, 'utf8'));
		for (const helper of HELPERS) {
			// A call, not the declaration (`function name(`) or an import.
			const re = new RegExp(`(?<!function\\s)(?<![\\w.$])${helper}\\(`, 'g');
			const n = (src.match(re) ?? []).length;
			if (n > 0) found[`${rel}::${helper}`] = n;
		}
		const lists = (src.match(DONE_LIST) ?? []).length;
		if (lists > 0) doneLists[rel] = lists;
	}

	it('classifies every call of a lifecycle helper', () => {
		const problems: string[] = [];
		for (const [key, n] of Object.entries(found)) {
			const site = SITES[key];
			if (!site) {
				problems.push(
					`unclassified: ${key} (${n} call(s)). Does it count open work or progress? Leave reference items out (isReferenceChild / collectionTracksWork) and classify it "exclude: <how>", otherwise "keep: <why>".`
				);
			} else if (site[0] !== n) {
				problems.push(`${key}: ${n} call(s), classified for ${site[0]}. Classify the new call(s).`);
			}
		}
		for (const key of Object.keys(SITES)) {
			if (!(key in found)) problems.push(`classified site no longer exists: ${key}`);
		}
		for (const [key, [, reason]] of Object.entries(SITES)) {
			if (!/^(exclude|keep): /.test(reason)) problems.push(`${key}: classification must start "exclude: " or "keep: "`);
		}
		expect(problems).toEqual([]);
	});

	it('finds no hardcoded done list outside the canonical default', () => {
		const extra = Object.entries(doneLists).filter(([file, n]) => n > (DONE_LISTS[file] ?? 0));
		expect(
			extra,
			'a literal done list drifts from each collection\'s own terminal options; use childProgress (childState / statusState)'
		).toEqual([]);
	});
});
