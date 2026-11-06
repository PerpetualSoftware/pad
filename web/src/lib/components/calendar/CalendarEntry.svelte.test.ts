import { expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import CalendarEntry from './CalendarEntry.svelte';
import type { CalendarRow } from '$lib/calendar/monthGrid';
const base: CalendarRow = { key: 'k', day: '2026-03-10', ref: 'TASK-1', target: 'TASK-1', title: 'Thing', allDay: true };
const show = (row: Partial<CalendarRow>) => render(CalendarEntry, { row: { ...base, ...row }, zone: 'UTC', basePath: '/w', onopen: vi.fn() });
const chipColor = (text: string) => (screen.getByText(text) as HTMLElement).style.getPropertyValue('--chip-c');
it('marks a delivered terminal row with a check', () => {
 show({ terminal: 'done', abandoned: false });
 expect(screen.getByLabelText('Done')).toHaveTextContent('✓');
});
it('marks a done schedule entry with a check', () => {
 show({ state: 'done' });
 expect(screen.getByLabelText('Done')).toHaveTextContent('✓');
});
it('shows a neutral dash, no check and the real label for an abandoned row', () => {
 const { container } = show({ terminal: 'abandoned', abandoned: true });
 expect(container.textContent).not.toContain('✓');
 expect(screen.queryByLabelText('Done')).toBeNull();
 const dash = screen.getByText('–', { exact: false, selector: 'span[aria-hidden="true"]' });
 expect(dash).toBeInTheDocument();
 expect(screen.getByText('Abandoned')).toBeInTheDocument();
});
it('keeps a custom abandoned status label and a neutral tone', () => {
 const { container } = show({ terminal: 'shelved', abandoned: true });
 expect(container.textContent).not.toContain('✓');
 expect(screen.getByText('Shelved')).toBeInTheDocument();
 expect(chipColor('Shelved')).toBe('var(--accent-gray)');
 expect(container.querySelector('span[aria-hidden="true"]')).not.toBeNull();
});
it('keeps the delivered tone for an unrecognised delivered status', () => {
 show({ terminal: 'shipped', abandoned: false });
 expect(chipColor('Shipped')).toBe('var(--accent-green)');
});
