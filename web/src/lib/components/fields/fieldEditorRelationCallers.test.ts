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

describe('CopyItemDialog stays on the read-only side of the gate', () => {
	// Not an oversight to fix later — the assertion IS the behaviour. Its
	// FieldDef comes from a preflight row that carries no `collection`
	// (`ItemCopyPreflightNeedsValue`), so a picker mounted here would be
	// unscoped, and this dialog copies ACROSS workspaces: it would offer
	// source-workspace items as the value for a destination-workspace field and
	// look authoritative doing it. TASK-2869 (U2b) fixes the contract end;
	// until then, no wsSlug here is what keeps the picker out.
	const tag = mountTag(copyDialog);

	it('does not pass a workspace slug', () => {
		expect(tag).not.toMatch(/\bwsSlug\b/);
	});

	it('still builds its FieldDef from a shape with no target collection', () => {
		// If this ever fails, the preflight row grew `collection` — which is
		// exactly U2b's change, and the gate above should be revisited WITH it
		// rather than left to drift.
		const toFieldDef = copyDialog.match(/function toFieldDef\([\s\S]*?\n\t\}/);
		expect(toFieldDef).not.toBeNull();
		expect(toFieldDef![0]).not.toMatch(/\bcollection\b/);
	});
});
