import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import MoveImpactDialog from './MoveImpactDialog.svelte';
import ResolveOccurrenceDialog from './ResolveOccurrenceDialog.svelte';
import AgendaDayDialog from './AgendaDayDialog.svelte';
import type { AgendaEntry, CommitmentMoveResult } from '$lib/types';

const entry: AgendaEntry = { type: 'schedule', state: 'outstanding', at: '2026-11-13T15:00:00Z', day: '2026-11-13', item_ref: 'TASK-9', item_title: 'Tide table import', collection: 'tasks', workspace: 'harbor', workspace_name: 'Harbor App', owner_username: 'alice', schedule_id: 's1' };
const root = { item_id: 'root', role: 'target' as const, from: '2026-11-13T15:00:00Z', to: '2026-11-20T15:00:00Z', root: true };
const linked = { item_id: 'linked', role: 'deadline' as const, from: '2026-11-18T14:00:00Z', to: '2026-11-25T14:00:00Z' };
const primaries = () => [...screen.getByRole('dialog').querySelectorAll('button.btn.primary')].map(button => button.textContent?.trim());
beforeEach(() => { vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-11-12T12:00:00Z')); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('MoveImpactDialog', () => {
	function mount(preview: Partial<CommitmentMoveResult> = {}, extra: Record<string, unknown> = {}) {
		const onsubmit = vi.fn(), onclose = vi.fn();
		render(MoveImpactDialog, { entry, preview: { revision: 'r1', changes: [linked, root], ...preview }, zone: 'America/New_York', titles: { linked: 'Publish price list' }, mobile: false, pending: false, error: '', onclose, onsubmit, ...extra });
		return { onsubmit, onclose, dialog: screen.getByRole('dialog', { name: 'Review move' }) };
	}
	it('names the item and lists what moves: title, role and short dates, the moved item first', () => {
		const { dialog } = mount();
		expect(dialog.querySelector('.subject')).toHaveTextContent('Tide table import · Harbor App TASK-9');
		const rows = [...dialog.querySelectorAll('[data-change]')].map(row => row.textContent?.replace(/\s+/g, ' ').trim());
		expect(rows).toEqual(['Tide table import Target Fri 13, 10:00 → Fri 20 Nov, 10:00', 'Publish price list Linked · Deadline Wed 18, 09:00 → Wed 25 Nov, 09:00']);
		expect(dialog.querySelector('.callout')).toBeNull(); expect(within(dialog).queryByText('Outside bound')).toBeNull();
		expect(within(dialog).queryByLabelText('Reason')).toBeNull(); expect(within(dialog).queryByRole('checkbox')).toBeNull();
	});
	it.each([[false, 'Move target'], [true, 'Move and change deadline']])('has one primary button, renegotiates=%s: %s', (renegotiates_deadline, label) => {
		mount({ renegotiates_deadline });
		expect(primaries()).toEqual([label]);
	});
	it('says a deadline change is special and requires a reason', async () => {
		const { dialog, onsubmit } = mount({ renegotiates_deadline: true });
		expect(dialog.querySelector('.callout')).toHaveTextContent('This move changes the hard deadline.');
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Move and change deadline' }));
		expect(within(dialog).getByRole('alert')).toHaveTextContent('Enter a reason.'); expect(onsubmit).not.toHaveBeenCalled();
		await fireEvent.input(within(dialog).getByLabelText('Reason'), { target: { value: ' Agreed later ' } });
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Move and change deadline' }));
		expect(onsubmit).toHaveBeenCalledWith('Agreed later', undefined);
	});
	it('marks a change outside its bound and requires the acknowledgement', async () => {
		const { dialog, onsubmit } = mount({ violations: [linked] });
		expect(dialog.querySelector('.callout')).toHaveTextContent('1 linked item would move outside its bounds.');
		const rows = [...dialog.querySelectorAll<HTMLElement>('[data-change]')];
		expect(within(rows[1]).getByText('Outside bound')).toBeInTheDocument(); expect(within(rows[0]).queryByText('Outside bound')).toBeNull();
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Move target' }));
		expect(within(dialog).getByRole('alert')).toHaveTextContent('Accept dates outside their bounds to move.'); expect(onsubmit).not.toHaveBeenCalled();
		await fireEvent.click(within(dialog).getByLabelText('Accept dates outside their bounds'));
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Move target' }));
		expect(onsubmit).toHaveBeenCalledWith(undefined, true);
	});
	it('names both when a move changes the deadline and breaks bounds', () => {
		const { dialog } = mount({ renegotiates_deadline: true, violations: [linked, { ...linked, item_id: 'other' }] });
		expect(dialog.querySelector('.callout')).toHaveTextContent('This move changes the hard deadline. 2 linked items would move outside their bounds.');
	});
	it('shows the error inside and ignores Escape, Close and Cancel while pending', async () => {
		const { dialog, onclose } = mount({}, { pending: true, error: 'Could not update this occurrence. Try again.' });
		expect(within(dialog).getByRole('alert')).toHaveTextContent('Could not update this occurrence. Try again.');
		await fireEvent(dialog, new Event('cancel', { cancelable: true }));
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Close' })); await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
		expect(onclose).not.toHaveBeenCalled();
	});
});

describe('ResolveOccurrenceDialog', () => {
	const mount = (mode: 'move' | 'snooze', extra: Record<string, unknown> = {}) => {
		const onclose = vi.fn();
		render(ResolveOccurrenceDialog, { open: true, mode, entry, zone: 'America/New_York', mobile: false, onclose, onsubmit: vi.fn(), ...extra });
		return onclose;
	};
	it.each([['move', 'Move target', 'Move target'], ['snooze', 'Snooze', 'Snooze']] as const)('%s is titled %s, names the item and has one primary button', (mode, title, label) => {
		mount(mode);
		expect(screen.getByRole('dialog', { name: title }).querySelector('.subject')).toHaveTextContent('Tide table import · Harbor App TASK-9');
		expect(primaries()).toEqual([label]);
		expect(screen.getByRole('button', { name: 'Cancel' })).toHaveClass('secondary');
	});
	it('ignores Escape and Cancel while pending', async () => {
		const onclose = mount('snooze', { pending: true });
		await fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
		expect(onclose).not.toHaveBeenCalled();
	});
});

describe('AgendaDayDialog', () => {
	const mount = (extra: Record<string, unknown> = {}) => render(AgendaDayDialog, { props: { day: '2026-11-12', today: '2026-11-12', entries: [entry, { ...entry, item_ref: 'TASK-10', schedule_id: 's2', state: 'done' }], zone: 'America/New_York', mobile: false, incomplete: false, pending: {}, saved: {}, onclose: vi.fn(), onaction: vi.fn(), ...extra } });
	it('is titled with the day, says Today, and summarises the day under it', () => {
		mount();
		const dialog = screen.getByRole('dialog', { name: 'Thursday, 12 November 2026, Today' });
		expect(dialog.querySelector('.subject')).toHaveTextContent('2 entries · 1 owed · 1 done');
		expect(dialog.textContent).not.toContain('Time zone'); expect(dialog.querySelector('footer')).toBeNull();
		expect([...dialog.querySelectorAll('[data-ref]')].map(row => row.getAttribute('data-ref'))).toEqual(['TASK-9', 'TASK-10']);
	});
	it('counts commitments only, says "1 entry", and ends with a collapsed disclosure of the completed entries', async () => {
		const completed = [{ ...entry, type: 'completed' as const, item_ref: 'TASK-20', state: undefined }, { ...entry, type: 'completed' as const, item_ref: 'TASK-21', state: undefined }];
		mount({ entries: [entry], completed });
		const dialog = screen.getByRole('dialog');
		expect(dialog.querySelector('.subject')).toHaveTextContent(/^1 entry · 1 owed$/);
		expect([...dialog.querySelectorAll('[data-ref]')].map(row => row.getAttribute('data-ref'))).toEqual(['TASK-9']);
		const toggle = within(dialog).getByRole('button', { name: '2 completed' });
		expect(toggle).toHaveAttribute('aria-expanded', 'false');
		await fireEvent.click(toggle);
		expect([...dialog.querySelectorAll('[data-ref]')].map(row => row.getAttribute('data-ref'))).toEqual(['TASK-9', 'TASK-20', 'TASK-21']);
	});
	it('shows the completed entries alone on a day with no commitments, with no subject and no empty line', () => {
		mount({ entries: [], completed: [{ ...entry, type: 'completed' as const, item_ref: 'TASK-20', state: undefined }] });
		const dialog = screen.getByRole('dialog');
		expect(dialog.querySelector('.subject')).toBeNull(); expect(within(dialog).queryByText('No entries on this day.')).toBeNull();
		expect(within(dialog).getByRole('button', { name: '1 completed' })).toBeInTheDocument();
	});
	it('leaves Today out on another day and keeps the incomplete and error lines', () => {
		mount({ today: '2026-11-13', entries: [], incomplete: true, error: 'Could not update this occurrence. Try again.' });
		const dialog = screen.getByRole('dialog', { name: 'Thursday, 12 November 2026' });
		expect(within(dialog).getByText('Not fully loaded')).toBeInTheDocument(); expect(within(dialog).queryByText('No entries on this day.')).toBeNull();
		expect(within(dialog).getByRole('alert')).toHaveTextContent('Could not update this occurrence. Try again.');
	});
});
