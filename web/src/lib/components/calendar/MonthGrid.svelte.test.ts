import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import MonthGrid from './MonthGrid.svelte';

const busy = new Set(['2026-11-05', '2026-11-12']);
function mount(extra: Record<string, unknown> = {}) {
	return render(MonthGrid, {
		month: '2026-11', today: '2026-11-12', selected: '2026-11-12', mobile: false, loaded: {},
		cellLabel: (day: string) => `Label ${day}${day === '2026-11-12' ? ', Today' : ''}`, summary: () => 'summary',
		count: (day: string) => busy.has(day) ? '6 · 2 owed' : '', empty: (day: string) => !busy.has(day),
		onopen: vi.fn(), onchoose: vi.fn(), cell: createRawSnippet(() => ({ render: () => '<i></i>' })), ...extra
	});
}
afterEach(cleanup);

describe('MonthGrid', () => {
	it('marks today with a badge and no Today word, and shows the day count beside the date', () => {
		mount();
		const today = document.querySelector('button.date[data-day="2026-11-12"]')!;
		expect(today).toHaveAttribute('aria-current', 'date'); expect(today).toHaveAttribute('aria-label', 'Label 2026-11-12, Today');
		expect(today.textContent?.trim()).toBe('12');
		const cell = today.closest('[role="gridcell"]')!;
		expect(cell).toHaveClass('today'); expect(cell.querySelector('.count')).toHaveTextContent('6 · 2 owed');
		expect(document.querySelector('button.date[data-day="2026-11-06"]')!.closest('[role="gridcell"]')!.querySelector('.count')).toBeNull();
	});
	it('keeps the arrow-key instruction for assistive technology only', () => {
		mount();
		const grid = screen.getByRole('grid', { name: 'November 2026' });
		const help = document.getElementById(grid.getAttribute('aria-describedby')!)!;
		expect(help).toHaveTextContent('Use arrow keys to choose a day. Press Enter to open its entries.');
		expect(help).toHaveClass('sr-only');
	});
	it('collapses runs of empty days on a phone and still lists every day with entries', () => {
		mount({ mobile: true });
		const list = screen.getByRole('list', { name: 'November 2026' });
		expect([...list.children].map(item => item.querySelector('button.day-header')?.getAttribute('data-day') ?? item.textContent?.trim())).toEqual([
			'Sun 1 – Wed 4 · nothing scheduled', '2026-11-05', 'Fri 6 – Wed 11 · nothing scheduled', '2026-11-12', 'Fri 13 – Mon 30 · nothing scheduled'
		]);
		expect(within(list).getAllByRole('button')).toHaveLength(2);
	});
	it('heads a phone day with the page\'s own heading and a Today badge when one is given', () => {
		mount({ mobile: true, heading: (day: string) => `Heading ${day}` });
		const today = document.querySelector('button.day-header[data-day="2026-11-12"]')!;
		expect(today.textContent?.replace(/\s+/g, ' ').trim()).toBe('Heading 2026-11-12 Today summary');
		expect(today.querySelector('.badge')).toHaveTextContent('Today');
		const other = document.querySelector('button.day-header[data-day="2026-11-05"]')!;
		expect(other.textContent?.replace(/\s+/g, ' ').trim()).toBe('Heading 2026-11-05 summary'); expect(other.querySelector('.badge')).toBeNull();
	});
	it('keeps the dated heading and the Today word when no heading is given', () => {
		mount({ mobile: true });
		expect(document.querySelector('button.day-header[data-day="2026-11-12"]')!.textContent?.replace(/\s+/g, ' ').trim()).toBe('Thursday, 12 November 2026 · Today summary');
	});
	it('never collapses today, a day that is not fully loaded or the selected day', () => {
		// The page reports a day that is not fully loaded as not empty.
		mount({ mobile: true, today: '2026-11-03', selected: '2026-11-20', empty: (day: string) => day !== '2026-11-25' });
		const list = screen.getByRole('list', { name: 'November 2026' });
		expect([...list.querySelectorAll('button.day-header')].map(button => button.getAttribute('data-day'))).toEqual(['2026-11-03', '2026-11-20', '2026-11-25']);
		expect(list.children).toHaveLength(7);
		expect(list.querySelector('button.day-header[data-day="2026-11-03"]')).toHaveAttribute('aria-current', 'date');
	});
});
