import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import StatusPicker from './collections/StatusPicker.svelte';
import { openStatusPicker, rowLabel } from './collections/statusPickerTestKit';
import PublicTableView from './share/PublicTableView.svelte';
import type { PublicCollection, PublicItem } from './share/shareView';

// TASK-3539: a keyed {#each} throws each_key_duplicate on a repeated key, in
// production builds too, and the whole view goes blank (BUG-3538). These
// render the shapes the census found reachable from stored data and assert
// they render, each value once.

afterEach(() => cleanup());

describe('repeated keys render instead of throwing (TASK-3539)', () => {
	it('StatusPicker with a repeated option opens, listing it once', async () => {
		const screen = render(StatusPicker, { props: { value: 'open', options: ['open', 'done', 'open'], onselect: vi.fn() } });
		const rows = await openStatusPicker(screen.container);
		expect(rows.map(rowLabel)).toEqual(['Open', 'Done']);
	});

	it('the public share table with a repeated field key renders one column for it', () => {
		const collection = {
			name: 'Cars',
			slug: 'cars',
			settings: {},
			fields: [
				{ key: 'colour', label: 'Colour', type: 'text' },
				{ key: 'colour', label: 'Colour again', type: 'text' },
				{ key: 'size', label: 'Size', type: 'text' },
			],
		} as unknown as PublicCollection;
		const item = { key: 'car-1', title: 'Car One', fields: { colour: 'red', size: 'l' }, tags: [] } as unknown as PublicItem;
		const screen = render(PublicTableView, { props: { collection, items: [item] } as never });
		expect(screen.container.textContent).toContain('Car One');
		expect(screen.container.textContent).toContain('Colour');
		expect(screen.container.textContent).not.toContain('Colour again');
	});
});
