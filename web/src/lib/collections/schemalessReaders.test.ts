// BUG-3067 — SOURCE-level guard that every by-name `status`/`priority` reader
// asks the schema, and that there is ONE implementation of the question.
//
// Why source-level rather than behavioural: the five surfaces are a class, and
// the claim worth pinning is a property of the POPULATION, not of any one
// render. `categoricalFieldValue.test.ts` proves the question is answered
// correctly; the per-surface render tests prove two of the surfaces call it.
// This proves that NO surface in the class answers it privately — which is the
// failure mode the class already has a history of, having been enumerated three
// times with a different scope each time (see the BUG-3067 trail).
//
// Same technique and the same reason as `laneKeyCallers.test.ts`: a mutant that
// re-inlines a correct private copy is invisible to every behavioural assertion
// in the suite, and "there is one copy" is a fact about the source.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');

/** Comments stripped, so a mention in prose is not read as a call. */
const code = (rel: string) =>
	read(rel)
		.replace(/<!--[\s\S]*?-->/g, '')
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.replace(/^[ \t]*\/\/.*$/gm, '');

const SURFACES = [
	{ name: 'ChildItems', src: '../components/ChildItems.svelte', keys: ['priority'] },
	{ name: 'NestedChildren', src: '../components/NestedChildren.svelte', keys: ['priority'] },
	{ name: 'CommandPalette', src: '../components/search/CommandPalette.svelte', keys: ['status', 'priority'] },
	{
		name: 'graph DetailCard',
		src: '../../routes/[username]/[workspace]/graph/DetailCard.svelte',
		keys: ['status', 'priority'],
	},
	// ADDED AFTER THE REVIEW ROUND FOUND IT (BUG-3067). I had classified this
	// page as a non-member because `playbooks` is a SYSTEM collection and I
	// assumed its schema could not be retyped — an assumption I wrote down as a
	// reason and never checked. `handleUpdateCollection` has no `is_system` gate
	// on schema edits, so it is a member like any other. The list is data now, so
	// a missed surface is a missing ROW rather than a silent absence.
	{
		name: 'playbooks page',
		src: '../../routes/[username]/[workspace]/playbooks/+page.svelte',
		keys: ['status'],
	},
	// ROUND 2 found three more, two of which are fixed here. Both render SERVER
	// PROJECTIONS — values the server itself obtained by name — which is why a
	// sweep for client-side `parseFields` reads kept missing them, and why the
	// class is stated as "a value obtained by name" rather than "a value read
	// from the fields blob".
	{
		name: 'dashboard cards',
		src: '../../routes/[username]/[workspace]/+page.svelte',
		keys: ['status', 'priority'],
	},
	{ name: 'ItemGraph', src: '../components/graph/ItemGraph.svelte', keys: ['status'] },
];

describe('every schema-less by-name reader routes through the shared question', () => {
	for (const s of SURFACES) {
		it(`${s.name} imports the shared helper`, () => {
			// ONE MODULE, not "either of two". The first version of this leg also
			// accepted an import from `shareView`, the lower-level helper — which
			// let a surface bypass the shared question while satisfying the guard,
			// and `graph/DetailCard` was doing exactly that for its `status` read
			// when the review round pointed it out. The bypass is the thing this
			// leg exists to prevent, so it cannot be an accepted spelling.
			const body = code(s.src);
			expect(
				/from '\$lib\/collections\/categoricalFieldValue'/.test(body),
				`${s.name} does not import the shared helper`,
			).toBe(true);
			expect(
				/categoricalChipValue/.test(body),
				`${s.name} reaches past the shared helper to its lower-level dependency`,
			).toBe(false);
		});

		it(`${s.name} renders no raw by-name read of ${s.keys.join('/')}`, () => {
			// ABOUT THE RENDER, not the read: passing `fields.priority` INTO the
			// helper is correct and necessary, so the guard cannot forbid the read
			// itself. It forbids the mustache `{fields.<key>}` going straight into
			// the markup, which is the shape the defect had at three of the four
			// surfaces.
			//
			// IT DOES NOT COVER `CommandPalette`, and saying so is the point:
			// that file renders `{meta.status}`, where `meta` is the OUTPUT of the
			// guarded function — forbidding that spelling would forbid the fix.
			// Measured: reverting `CommandPalette` to the unfixed tree fails the
			// IMPORT leg above and passes this one, so the import leg is what
			// covers that surface. Two legs, different reach, neither redundant.
			const body = code(s.src);
			for (const key of s.keys) {
				expect(
					body.includes(`{fields.${key}}`),
					`${s.name} prints fields.${key} directly — a retyped field renders its id`,
				).toBe(false);
			}
		});
	}

	it('the shared helper exists and asks the FIELD TYPE, not the value shape', () => {
		// The question has one home, and that home consults `isRelationType`
		// (through `categoricalChipValue`) rather than testing whether the value
		// happens to look like an id. A guard on the value's shape is what every
		// one of these sites already had, and it is what let the defect through:
		// a scalar relation's value IS a string.
		const helper = code('./categoricalFieldValue.ts');
		expect(helper).toContain('categoricalChipValue');
		expect(helper).toContain('parseSchema');
	});
});
