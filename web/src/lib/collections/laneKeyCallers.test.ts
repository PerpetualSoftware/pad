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

/**
 * The whole import STATEMENT, not the line the path sits on.
 *
 * The first version of this read a single line and asked whether it contained
 * each name — which worked until a surface's import grew past the line length
 * and Prettier-style formatting split it, at which point the matched line was
 * `} from '$lib/collections/boardColumns';` and contained no names at all. The
 * guard went red on an unmutated tree and "killed" an unrelated mutant while it
 * was there. An instrument keyed on formatting measures the formatting.
 */
function boardColumnsImport(source: string): string | null {
	const m = source.match(/import\s*\{([\s\S]*?)\}\s*from\s*'\$lib\/collections\/boardColumns'/);
	return m ? m[1] : null;
}

describe('every grouped surface imports the shared lane-key helpers', () => {
	for (const surface of SURFACES) {
		it(`${surface.name} imports what it uses from boardColumns`, () => {
			const names = boardColumnsImport(surface.source);
			expect(names, `${surface.name} should import from boardColumns`).toBeTruthy();
			for (const name of surface.uses) {
				expect(names, `${surface.name} should import ${name}`).toContain(name);
			}
		});
	}

	it('reads a MULTI-LINE import too', () => {
		// Control: the shape that broke the first version.
		const multi = [
			'\timport {',
			'\t\tbucketByColumn,',
			'\t\tformatLaneLabel,',
			"\t} from '$lib/collections/boardColumns';",
		].join('\n');
		expect(boardColumnsImport(multi)).toContain('formatLaneLabel');
		expect(boardColumnsImport('nothing here')).toBeNull();
	});
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
		// Strip the whole import statement, so importing a shared name never reads
		// as redefining it — for a one-line or a wrapped import alike.
		const body = surface.source.replace(
			/import\s*\{[\s\S]*?\}\s*from\s*'\$lib\/collections\/boardColumns';/,
			'',
		);

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

/**
 * The WRITE side of the same seam (BUG-3057).
 *
 * BUG-3053 closed the read side — comparing a lane key against a raw value —
 * and this file's guards are about surfaces READING through the shared helpers.
 * The two write paths on the collection page had the mirror defect: they
 * assigned the lane KEY back to the field, whatever its declared type, and the
 * server refuses those writes (`"0"` into a number, `"c"` into a multi_select),
 * so a legitimate drag or create-in-lane simply failed.
 *
 * Asserted at the SOURCE for the reason the guards above are: a behaviour test
 * of `laneWriteValue` stays green if a call site stops calling it, and the
 * call sites live in a 2000-line route component whose write paths need a
 * board, a drag and a network mock to reach.
 */
describe('BUG-3057: the collection page writes a CONVERTED lane value', () => {
	const page = read('../../routes/[username]/[workspace]/[collection]/+page.svelte');

	it('imports the converter', () => {
		expect(page).toContain("from '$lib/collections/laneWriteValue'");
		expect(page).toContain('laneWriteValue');
		expect(page).toContain('laneWriteRefusalMessage');
	});

	it('sends the CONVERTED value on the drag path, not the lane key', () => {
		// The mutant this kills is the original line, restored. The patched key
		// is `fieldKey` rather than `groupField` because the writer now takes its
		// target explicitly — see the status-chip leg below.
		expect(page).toContain('const fieldsPatch = { [fieldKey]: laneWrite.value };');
		expect(page).not.toContain('const fieldsPatch = { [groupField]: newValue };');
	});

	it('lets EVERY status chip name the STATUS field, not the lane field', () => {
		// The table has no lanes: its chip cycles the `status` schema field's
		// options and used to send them to `groupField`, which in table view is
		// `list_group_by`. On a table grouped by `priority`, a status click set
		// the priority (BUG-3057). The list and the board had the same defect
		// through a shared prop, fixed in BUG-3068 by splitting the drop's writer
		// (`onLaneChange`) from the chip's (`onStatusChange`).
		//
		// COUNTING IS LOAD-BEARING HERE. This leg was a single `toContain` of the
		// table's line, and BUG-3068 made two more sites carry that exact string
		// — so it would have gone on passing if either of the new ones had been
		// wired wrongly, or if the table's had been deleted while a new one
		// remained. A `toContain` cannot tell one occurrence from three.
		const statusNamed =
			"onStatusChange={(it, newStatus) => handleStatusChange(it, newStatus, 'status')}";
		const occurrences = page.split(statusNamed).length - 1;
		expect(
			occurrences,
			'expected exactly three chip call sites naming status: BoardView, TableView, ListView',
		).toBe(3);

		// And each of the two LANE writers still targets the group field, which is
		// what the default argument is for. If a view lost its lane writer the
		// drop would silently start writing `status`.
		expect(page.split('onLaneChange={handleStatusChange}').length - 1).toBe(2);

		expect(page).toContain(
			'async function handleStatusChange(item: Item, newValue: string, fieldKey: string = groupField)',
		);
	});

	it('sends the CONVERTED value on the create-in-lane path, not the lane key', () => {
		expect(page).toContain('laneWriteValue(groupFieldDef, groupValue)');
		expect(page).not.toContain('defaultFields[groupField] = groupValue;');
	});

	it('resolves the group field against the SCHEMA, since the type is what converts', () => {
		// Without this derive, both call sites would have nothing to convert
		// through and the converter would fall into its undeclared-field arm —
		// which writes the key, i.e. the defect, with a helper in front of it.
		expect(page).toContain("let groupFieldDef = $derived(schema?.fields.find((f) => f.key === groupField));");
	});
});
