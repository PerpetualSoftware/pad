import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import ScheduleEndDialog from './ScheduleEndDialog.svelte';
import type { AgendaEntry, ItemStillOpenDetails } from '$lib/types';

const entry: AgendaEntry = { type: 'schedule', state: 'outstanding', at: '2026-11-13T15:00:00Z', day: '2026-11-13', item_ref: 'TASK-9', item_title: 'Renew passport', collection: 'tasks', workspace: 'home', workspace_name: 'Home', owner_username: 'alice', schedule_id: 's1' };
const details = (extra: Partial<ItemStillOpenDetails> = {}): ItemStillOpenDetails => ({ state: 'done', occurrence_at: entry.at, done_field: 'status', item_status: 'open', close_options: ['done'], ack_available: false, ...extra });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function mount(d: ItemStillOpenDetails, extra: Record<string, unknown> = {}) {
	const onchoose = vi.fn(), onclose = vi.fn();
	render(ScheduleEndDialog, { open: true, entry, details: d, mobile: false, onclose, onchoose, ...extra });
	const dialog = screen.getByRole('dialog', { name: 'Last occurrence: Renew passport is still open' });
	const choices = () => [...dialog.querySelectorAll<HTMLElement>('[data-choice]')].map(el => el.dataset.choice);
	return { dialog, onchoose, onclose, choices };
}

describe('ScheduleEndDialog', () => {
	it('offers the choices the details allow', () => {
		expect(mount(details()).choices()).toEqual(['close', 'end']); cleanup();
		expect(mount(details({ ack_available: true })).choices()).toEqual(['close', 'end', 'ack']); cleanup();
		// A skip never closes the item; a field with no delivering close has nothing to close with.
		expect(mount(details({ state: 'skip', ack_available: true })).choices()).toEqual(['end', 'ack']); cleanup();
		expect(mount(details({ close_options: [] })).choices()).toEqual(['end']);
	});
	it('names the item and its status, and says nothing is saved yet', () => {
		const { dialog } = mount(details({ item_status: 'in-progress' }));
		expect(dialog).toHaveTextContent('The item is still open (in-progress). Nothing has been saved yet.');
	});
	it('closes with the only option by default', async () => {
		const { dialog, onchoose } = mount(details());
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Close item' }));
		expect(onchoose).toHaveBeenCalledWith({ kind: 'close', value: 'done' });
	});
	it('asks which close when there is more than one', async () => {
		const { dialog, onchoose } = mount(details({ close_options: ['shipped', 'done'] }));
		await fireEvent.change(within(dialog).getByLabelText('Close as'), { target: { value: 'done' } });
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Close item' }));
		expect(onchoose).toHaveBeenCalledWith({ kind: 'close', value: 'done' });
	});
	it('ends the schedule or acknowledges, and Cancel only closes', async () => {
		const { dialog, onchoose, onclose } = mount(details({ ack_available: true }));
		await fireEvent.click(within(dialog).getByLabelText(/End schedule, keep item open/));
		await fireEvent.click(within(dialog).getByRole('button', { name: 'End schedule' }));
		expect(onchoose).toHaveBeenLastCalledWith({ kind: 'end' });
		await fireEvent.click(within(dialog).getByLabelText(/Acknowledge instead/));
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Acknowledge' }));
		expect(onchoose).toHaveBeenLastCalledWith({ kind: 'ack' });
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
		expect(onclose).toHaveBeenCalledTimes(1); expect(onchoose).toHaveBeenCalledTimes(2);
	});
	it('shows an error inside and disables its buttons while pending', () => {
		const { dialog } = mount(details(), { pending: true, error: 'Could not update this occurrence. Try again.' });
		expect(within(dialog).getByRole('alert')).toHaveTextContent('Could not update this occurrence. Try again.');
		for (const name of ['Cancel', 'Close item']) expect((within(dialog).getByRole('button', { name }) as HTMLButtonElement).disabled).toBe(true);
	});
});
