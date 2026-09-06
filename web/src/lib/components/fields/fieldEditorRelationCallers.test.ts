// Node-project test (no DOM): SOURCE-level guards on the two live call sites
// of `fields/FieldEditor.svelte` (TASK-2868).
//
// The relation branch is gated on `wsSlug` AND `field.collection`, and the two
// callers sit on opposite sides of that gate ON PURPOSE. Neither property is
// visible from FieldEditor's own render tests — they are facts about who mounts
// it — so they are asserted where they live, the same shape as
// `itemDetailUsesPicker.test.ts`.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const read = (rel: string) =>
	readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8').replace(/<!--[\s\S]*?-->/g, '');

const itemDetail = read('../items/ItemDetail.svelte');
const copyDialog = read('../items/CopyItemDialog.svelte');

function mountTag(source: string): string {
	const m = source.match(/<FieldEditor\b[\s\S]*?\/>/);
	expect(m, '<FieldEditor …/> not found').not.toBeNull();
	return m![0];
}

describe('ItemDetail gives FieldEditor the relation link context', () => {
	const tag = mountTag(itemDetail);

	it('passes the workspace slug — without it the relation branch is read-only', () => {
		expect(tag).toMatch(/\{wsSlug\}/);
	});

	it('passes the username, which is what builds the chip href', () => {
		expect(tag).toMatch(/\{username\}/);
	});

	it('wires the pane-open target so the chip peeks like every other item link', () => {
		expect(tag).toMatch(/onOpenTarget=\{paneOpenTarget\}/);
	});
});

describe('CopyItemDialog is now on the EDITABLE side of the gate (TASK-2869 / U2b)', () => {
	// THIS BLOCK USED TO ASSERT THE OPPOSITE, and that was correct at the time:
	// the preflight row carried no `collection`, so a picker mounted here would
	// have been unscoped — and this dialog copies ACROSS workspaces, so it would
	// have offered SOURCE-workspace items as the value for a DESTINATION field
	// and looked authoritative doing it. Withholding `wsSlug` is what kept the
	// picker out.
	//
	// U2b closed the contract end the old block named by ref: the row now
	// carries its target collection, so both halves of FieldEditor's gate can
	// be satisfied honestly. The old version told its successor to revisit the
	// gate WITH that change rather than let it drift, which is this block.
	const tag = mountTag(copyDialog);

	it('passes the DESTINATION workspace slug, not the source', () => {
		// destWs, never sourceWsSlug: a relation resolves at the destination,
		// so the picker must list items the copy can actually point at.
		expect(tag).toMatch(/wsSlug=\{destWs\}/);
		expect(tag).not.toMatch(/wsSlug=\{sourceWsSlug\}/);
	});

	it('carries the row\'s target collection into the FieldDef', () => {
		const toFieldDef = copyDialog.match(/function toFieldDef\([\s\S]*?\n\t\}/);
		expect(toFieldDef).not.toBeNull();
		expect(toFieldDef![0]).toMatch(/collection: row\.collection/);
	});

	it('still refuses to mount a picker for a relation row with NO collection', () => {
		// The other half of the gate, and the half that is easy to lose: the
		// dialog delegates that decision to isCollectable, which is unit-tested
		// in copyNeedsValue.test.ts. Asserting the DELEGATION here means the
		// two cannot drift apart — a dialog that inlined its own predicate
		// again would pass those unit tests and fail this.
		expect(copyDialog).toMatch(/from '\$lib\/items\/copyNeedsValue'/);
		expect(copyDialog).toMatch(/\{#if isCollectable\(row\)\}/);
	});
});

