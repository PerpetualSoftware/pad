// Node-project test (no DOM): SOURCE-level guard that every create path takes
// its status default from `createDefaults` rather than reading `options[0]`
// itself (BUG-3078).
//
// This is the defect's own shape, so it is the thing worth pinning. The bug was
// not a wrong line — it was SIX copies of one question ("what status should a
// new item start with?") in six files, none of which asked whether the field
// could hold the answer. A behaviour test at one site says nothing about the
// other five, and a mutant that re-inlines the read at a site with no behaviour
// test of its own is invisible to every assertion in the suite. The claim "there
// is one copy" is a fact about the SOURCE, so it is asserted where it lives —
// the same shape as `laneKeyCallers.test.ts`, which exists for the sibling
// question about a lane KEY.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');

/** Every surface that creates an item with a default status. */
const CALLERS: { name: string; rel: string; expectedCalls: number }[] = [
	{ name: 'QuickCaptureSheet', rel: '../components/layout/QuickCaptureSheet.svelte', expectedCalls: 1 },
	{ name: 'Sidebar quick-add', rel: '../components/layout/Sidebar.svelte', expectedCalls: 1 },
	{ name: 'EditorBubbleMenu', rel: '../components/editor/EditorBubbleMenu.svelte', expectedCalls: 1 },
	// Three separate create paths in one file: "Untitled", create-in-lane, and
	// quick-create. Counted rather than merely detected, so removing two of the
	// three still fails. The fourth (BUG-3043) is `draftPlacement`, which asks
	// the same defaults whether an omitted group key would be filled in.
	{ name: 'collection page', rel: '../../routes/[username]/[workspace]/[collection]/+page.svelte', expectedCalls: 4 },
];

/**
 * The shape the fix removed, written to match the VARIANTS rather than the one
 * spelling that happened to be there.
 *
 * Three of the six wrote `f => f.key` and three wrote `(f) => f.key`; two used a
 * local named `defaultFields` and one used `fields`. A guard keyed on one
 * spelling would have passed on the unfixed tree for half the sites, which is
 * the same mistake the code made. This keys on the ASSIGNMENT — anything
 * reading an `options[0]` off a status-ish field def into a result object.
 */
const INLINE_READ = /\.options\s*(\?\.)?\[\s*0\s*\]/;

describe('every create path takes its status default from createDefaults', () => {
	for (const caller of CALLERS) {
		it(`${caller.name} calls createDefaultFields ${caller.expectedCalls}x`, () => {
			const src = read(caller.rel);
			expect(src, `${caller.name} should import createDefaultFields`).toContain(
				"from '$lib/collections/createDefaults'",
			);
			const calls = src.match(/createDefaultFields\s*\(/g) ?? [];
			expect(calls.length, `${caller.name} call count`).toBe(caller.expectedCalls);
		});

		it(`${caller.name} reads no options[0] of its own`, () => {
			expect(INLINE_READ.test(read(caller.rel)), `${caller.name} has an inline options[0]`).toBe(
				false,
			);
		});
	}

	it('the inline-read pattern matches every spelling the six sites used', () => {
		// Positive control. Without this the guard above could be a regex that
		// matches nothing, which passes on any tree and proves nothing — the
		// failure mode that makes an absence assertion vacuous.
		const spellings = [
			"defaultFields.status = statusField.options[0];",
			"fields.status = field.options[0];",
			"defaultFields.status = statusField.options?.[0];",
			"defaultFields.status = statusField.options[ 0 ];",
		];
		for (const s of spellings) {
			expect(INLINE_READ.test(s), s).toBe(true);
		}
	});

	it('the inline-read pattern does not match the module that legitimately reads options', () => {
		// Negative control, and a real one: `createDefaults.ts` and
		// `laneWriteValue.ts` both index `options`, and a guard that flagged them
		// would have to be weakened until it flagged nothing.
		expect(INLINE_READ.test('const first = statusField?.options?.[0];')).toBe(true);
		expect(read('./createDefaults.ts')).toContain('options?.[0]');
		// ...which is exactly why the guard is scoped to the CALLERS list above
		// and never swept across the tree.
		expect(CALLERS.some((c) => c.rel.includes('createDefaults'))).toBe(false);
	});
});
