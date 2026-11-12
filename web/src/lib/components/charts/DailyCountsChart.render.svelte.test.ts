import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import DailyCountsChart from './DailyCountsChart.svelte';

afterEach(cleanup);

describe('daily counts chart', () => {
 it('draws bars and two lines, and exposes local days and all values in a table', () => {
  const { container } = render(DailyCountsChart, { props: { data: [
   { day: '2026-11-05', completed: 2, open: 10, burndown: 8 },
   { day: '2026-11-06', completed: 3, open: 9, burndown: 6 },
   { day: '2026-11-07', completed: 4, open: 7, burndown: 3 }
  ] } });
  expect(screen.getByRole('img', { name: 'Daily counts' })).toBeTruthy();
  const table = screen.getByRole('table', { name: 'Daily counts' });
  expect({
   caption: table.querySelector('caption')?.textContent,
   rows: Array.from(table.querySelectorAll('tr'), row => Array.from(row.querySelectorAll('th, td'), cell => cell.textContent?.trim())),
   legend: Array.from(container.querySelectorAll('.legend-item'), item => item.textContent?.trim())
  }).toMatchInlineSnapshot(`
   {
     "caption": "Daily counts",
     "legend": [
       "Completed",
       "Open at day end",
       "Burn-down",
     ],
     "rows": [
       [
         "Day",
         "Completed",
         "Open at day end",
         "Burn-down",
       ],
       [
         "Nov 5",
         "2",
         "10",
         "8",
       ],
       [
         "Nov 6",
         "3",
         "9",
         "6",
       ],
       [
         "Nov 7",
         "4",
         "7",
         "3",
       ],
     ],
   }
  `);
  expect(container.querySelectorAll('g.bars rect:not(.hit)')).toHaveLength(3);
  const lines = container.querySelectorAll('g.lines path');
  expect(lines).toHaveLength(2);
  for (const line of lines) {
   expect(line.getAttribute('d')).toMatch(/^M.+L.+L/);
   expect(line.getAttribute('d')).not.toContain('NaN');
  }
 });
});
