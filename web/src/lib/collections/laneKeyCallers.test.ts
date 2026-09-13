// Node-project test (no DOM): SOURCE-level guard that every surface which groups
// items reads the SHARED lane-key helpers rather than a private copy (BUG-3053).
//
// This is the defect's own shape, so it is the thing worth pinning. The bug was
// not a wrong line — it was TWO copies of one question ("does this item have a
// group value?") that had drifted to different answers: ListView tested the RAW
// field for falsiness in its lane pass while bucketing under the STRINGIFIED
// value, so an item scoring 0 was filed under '0' with no lane pointing there
// and vanished. By the time it was found there were FOUR private spellings of
// the normalise-and-ask pair across three surfaces, plus two byte-identical
// private `formatLabel`s.
//
// Consolidation cannot be measured by a behaviour test — that is the point of a
// refactor, and a mutant that re-inlines a correct copy is invisible to every
// assertion in the suite (E12 on the BUG-3053 trail). The claim "there is one
// copy" is a fact about the SOURCE, so it is asserted where it lives, the same
// shape as `fieldEditorRelationCallers.test.ts`.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');

/**
 * `forbidden` is scoped per surface on purpose.
 *
 * shareView keeps its OWN `formatLabel`, and that is correct rather than an
 * oversight: it title-cases field KEYS as well as values, strips hyphens as
 * well as underscores, and has no Uncategorized case, so it is a different
 * function that happens to share a name. Only the two in-app views carried a
 * byte-identical copy of the LANE label, and only they are forbidden one.
 */
const SURFACES: { name: string; source: string; uses: string[]; forbidden: string[] }[] = [
	{
		name: 'ListView',
		source: read('../components/collections/ListView.svelte'),
		uses: ['laneValue', 'isUngrouped', 'formatLaneLabel'],
		forbidden: ['formatLabel', 'laneValue', 'isUngrouped', 'inlinedLaneValue'],
	},
	{
		name: 'BoardView',
		source: read('../components/collections/BoardView.svelte'),
		// Buckets through `bucketByColumn`, which normalises internally, so it
		// needs the label but not the pair.
		uses: ['formatLaneLabel'],
		forbidden: ['formatLabel', 'laneValue', 'isUngrouped', 'inlinedLaneValue'],
	},
	{
		name: 'shareView (public board + list)',
		source: read('../components/share/shareView.ts'),
		uses: ['laneValue', 'isUngrouped'],
		forbidden: ['laneValue', 'isUngrouped', 'inlinedLaneValue'],
	},
];

describe('every grouped surface imports the shared lane-key helpers', () => {
	for (const surface of SURFACES) {
		it(`${surface.name} imports what it uses from boardColumns`, () => {
			const importLine = surface.source
				.split('\n')
				.find((l) => l.includes("from '$lib/collections/boardColumns'"));
			expect(importLine, `${surface.name} should import from boardColumns`).toBeTruthy();
			for (const name of surface.uses) {
				expect(importLine, `${surface.name} should import ${name}`).toContain(name);
			}
		});
	}
});

describe('no surface keeps a private copy of a lane-key helper', () => {
	// Each pattern is the SHAPE the private copy had before consolidation. They
	// are matched against the source with its own import line excluded, so
	// importing the shared name never looks like redefining it.
	const PATTERNS: Record<string, { pattern: RegExp; what: string }> = {
		formatLabel: { pattern: /function\s+formatLabel\s*\(/, what: 'a private formatLabel' },
		laneValue: { pattern: /function\s+laneValue\s*\(/, what: 'a private laneValue' },
		isUngrouped: { pattern: /function\s+isUngrouped\s*\(/, what: 'a private isUngrouped' },
		inlinedLaneValue: {
			// The inlined normalisation, in the exact spelling all four copies used.
			pattern: /typeof\s+\w+\s*===\s*'string'\s*\?\s*\w+\s*:\s*\w+\s*==\s*null\s*\?\s*''/,
			what: 'an inlined laneValue',
		},
	};

	for (const surface of SURFACES) {
		const body = surface.source
			.split('\n')
			.filter((l) => !l.includes("from '$lib/collections/boardColumns'"))
			.join('\n');

		for (const key of surface.forbidden) {
			const { pattern, what } = PATTERNS[key];
			it(`${surface.name} does not define ${what}`, () => {
				expect(pattern.test(body), `${surface.name} still defines ${what}`).toBe(false);
			});
		}
	}

	it('the guard can actually fire', () => {
		// Control: the patterns must match the code they describe, or every
		// assertion above is a green that proves nothing. These are the literal
		// shapes that were deleted from the three surfaces.
		expect(/function\s+formatLabel\s*\(/.test('\tfunction formatLabel(value: string): string {')).toBe(
			true,
		);
		expect(
			/typeof\s+\w+\s*===\s*'string'\s*\?\s*\w+\s*:\s*\w+\s*==\s*null\s*\?\s*''/.test(
				"const value = typeof raw === 'string' ? raw : raw == null ? '' : String(raw);",
			),
		).toBe(true);
	});
});
