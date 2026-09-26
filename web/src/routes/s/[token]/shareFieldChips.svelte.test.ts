// TASK-2248 U3 (audit C121): a direct item share renders its field chips by the
// same rule as the collection inline-expand (labelled, coloured, relations as
// a note) whenever the server sends defs, and exactly as before when it does
// not (an older server).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { fieldChips } from '$lib/components/share/shareView';
import type { FieldDef } from '$lib/types';

const share = vi.hoisted(() => ({ payload: null as unknown }));
vi.mock('$lib/api/client', () => ({
	api: { share: { get: vi.fn(async () => share.payload) } },
	withRequestDeadline: vi.fn(),
}));

import { page } from '$app/state';
import SharePage from './+page.svelte';

afterEach(() => cleanup());

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

const defs = [
	{ key: 'status', label: 'Status', type: 'select', terminal_options: ['shipped'] },
	{ key: 'owner', label: 'Owner', type: 'relation' },
	{ key: 'effort', label: 'Effort', type: 'select' },
	{ key: 'hours', label: 'Hours', type: 'number', suffix: 'h' },
] as FieldDef[];

describe('fieldChips: one rule for both share views', () => {
	it('labels a categorical value, colours a terminal one, notes a relation, keeps a literal verbatim', () => {
		const chips = fieldChips({ status: 'shipped', owner: 'u-123', effort: 'in_progress', hours: 3, extra: 'x' }, defs);
		expect(chips.map((c) => [c.field.key, c.text, c.placeholder])).toEqual([
			['status', 'Shipped', false],
			['owner', expect.not.stringContaining('u-123'), true],
			['effort', 'In Progress', false],
			['hours', '3 h', false],
		]);
		expect(chips[0].color).toBe('var(--accent-green)');
		// An undeclared key has no def, so it is not a chip.
		expect(chips.some((c) => c.field.key === 'extra')).toBe(false);
	});

	it('a computed field is not a chip', () => {
		expect(fieldChips({ score: 7 }, [{ key: 'score', label: 'Score', type: 'number', computed: true } as FieldDef])).toEqual([]);
	});
});

describe('share page: direct item field chips (C121)', () => {
	function mount(extra: Record<string, unknown>) {
		page.params = { token: 'tok' };
		share.payload = {
			type: 'item',
			item: { title: 'Feature', ref: 'FEAT-1', fields: JSON.stringify({ status: 'shipped', effort: 'in_progress', owner: 'u-123' }), content: '' },
			...extra,
		};
		return render(SharePage);
	}
	const chipTexts = (c: HTMLElement) => [...c.querySelectorAll('.item-fields .field-chip')].map((el) => el.textContent?.replace(/\s+/g, ' ').trim());

	it('with defs from the server: labelled, coloured, the relation as a note and never its id', async () => {
		const { container } = mount({ field_defs: defs });
		await settle();
		expect(chipTexts(container)[0]).toBe('Status Shipped');
		expect(chipTexts(container)).toContain('Effort In Progress');
		expect(container.querySelector('.item-fields')!.textContent).not.toContain('u-123');
		const status = [...container.querySelectorAll('.field-chip-value')].find((d) => d.textContent === 'Shipped') as HTMLElement;
		expect(status.style.color).toBe('var(--accent-green)');
	});

	it('without defs (an older server): the raw values, as before', async () => {
		const { container } = mount({});
		await settle();
		expect(chipTexts(container)).toContain('Effort in_progress');
	});
});
