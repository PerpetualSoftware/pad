// A PUBLIC share surface never prints a relation's stored id (BUG-3016).
//
// A share payload carries field VALUES with no index behind them, which is why
// U7 refused relation GROUPING on the public board and list rather than
// labelling lanes with ids. A COLUMN is not refusable the same way — hiding it
// silently changes what the owner chose to share — so the cell says what it
// holds instead: a linked item this share cannot resolve.
//
// The placeholder is a BOUNDARY, not a shrug (lead ruling). Resolving it is a
// VISIBILITY question — the target may live in a collection the owner never
// shared, and the viewer is anonymous — so it needs the per-target redaction
// tracked in IDEA-3066, not a payload field.
//
// Both surfaces are exercised here because they are two doors onto the same
// defect and were fixed from one helper; a test of the helper alone would not
// notice either door reverting to `formatFieldValue` (CONVE-19).
import { afterEach, describe, expect, it } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import PublicTableView from './PublicTableView.svelte';
import PublicItemExpansion from './PublicItemExpansion.svelte';
import type { PublicCollection, PublicItem } from './shareView';

const RELATION_ID = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';
const OTHER_ID = '9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d';

afterEach(() => {
	cleanup();
});

function collection(fields: unknown[]): PublicCollection {
	return { name: 'Cars', slug: 'cars', fields, settings: {} } as unknown as PublicCollection;
}

function item(fields: Record<string, unknown>): PublicItem {
	return { key: 'car-1', title: 'Car One', fields, tags: [] } as unknown as PublicItem;
}

const SCALAR = collection([{ key: 'car_color', label: 'Colour', type: 'relation' }]);
const MULTI = collection([{ key: 'car_colors', label: 'Colours', type: 'multi_relation' }]);

describe('PublicTableView', () => {
	it('renders a placeholder for a relation, never the id', () => {
		const screen = render(PublicTableView, {
			props: { collection: SCALAR, items: [item({ car_color: RELATION_ID })] } as never,
		});
		expect(screen.container.textContent).toContain('Linked item');
		expect(screen.container.textContent).not.toContain(RELATION_ID);
	});

	it('counts the elements of a multi_relation without naming any of them', () => {
		// The COUNT describes the shared item's own field, not any unshared
		// target, so it is not part of the exposure the placeholder exists for.
		const screen = render(PublicTableView, {
			props: { collection: MULTI, items: [item({ car_colors: [RELATION_ID, OTHER_ID] })] } as never,
		});
		expect(screen.container.textContent).toContain('2 linked items');
		expect(screen.container.textContent).not.toContain(RELATION_ID);
		expect(screen.container.textContent).not.toContain(OTHER_ID);
	});

	it('leaves an empty relation cell empty rather than claiming a link', () => {
		const screen = render(PublicTableView, {
			props: { collection: SCALAR, items: [item({})] } as never,
		});
		// PRECONDITION: the row is there, so "no placeholder" is not "no table".
		expect(screen.container.textContent).toContain('Car One');
		expect(screen.container.textContent).not.toContain('Linked item');
	});

	it('CONTROL: an ordinary text field still prints its value verbatim', () => {
		// Without this, a fix that blanked every cell would satisfy the legs
		// above. The value is id-SHAPED on purpose: it separates "this field is a
		// relation" from "this string looks like an id".
		const text = collection([{ key: 'vin', label: 'VIN', type: 'text' }]);
		const screen = render(PublicTableView, {
			props: { collection: text, items: [item({ vin: RELATION_ID })] } as never,
		});
		expect(screen.container.textContent).toContain(RELATION_ID);
		expect(screen.container.textContent).not.toContain('Linked item');
	});
});

describe('PublicItemExpansion', () => {
	it('renders a placeholder for a relation, never the id', () => {
		const screen = render(PublicItemExpansion, {
			props: {
				item: item({ car_color: RELATION_ID }),
				fields: (SCALAR as unknown as { fields: unknown[] }).fields,
				html: '',
				id: 'x',
			} as never,
		});
		expect(screen.container.textContent).toContain('Linked item');
		expect(screen.container.textContent).not.toContain(RELATION_ID);
	});

	it('CONTROL: an ordinary text field still prints its value verbatim', () => {
		const text = collection([{ key: 'vin', label: 'VIN', type: 'text' }]);
		const screen = render(PublicItemExpansion, {
			props: {
				item: item({ vin: RELATION_ID }),
				fields: (text as unknown as { fields: unknown[] }).fields,
				html: '',
				id: 'x',
			} as never,
		});
		expect(screen.container.textContent).toContain(RELATION_ID);
		expect(screen.container.textContent).not.toContain('Linked item');
	});
});
