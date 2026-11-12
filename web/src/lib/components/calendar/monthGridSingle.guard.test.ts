import { expect, it } from 'vitest';

it('keeps the sole month grid in MonthGrid.svelte', () => {
	const sources = import.meta.glob<string>('/src/**/*.svelte', { query: '?raw', import: 'default', eager: true });
	expect(Object.entries(sources).filter(([, source]) => source.includes('role="grid"')).map(([path]) => path))
		.toEqual(['/src/lib/components/calendar/MonthGrid.svelte']);
});
