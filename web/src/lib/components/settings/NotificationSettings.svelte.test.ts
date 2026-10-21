import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/**
 * TASK-3940: the Notifications settings section. Every save is a full replace
 * of seven values, so these tests read the PUT bodies the component sent.
 */
const KEYS = [
	'push_enabled',
	'notify_assigned',
	'notify_comments',
	'notify_replies',
	'notify_reminders',
	'notify_mentions',
	'include_agent_activity'
] as const;
const LABELS: Record<(typeof KEYS)[number], string> = {
	push_enabled: 'Push notifications',
	notify_assigned: 'Assigned to you',
	notify_comments: 'Comments on your items',
	notify_replies: 'Replies to your comments',
	notify_reminders: 'Reminders',
	notify_mentions: 'Mentions',
	include_agent_activity: 'Include activity by agents'
};

const state = vi.hoisted(() => ({
	server: {} as Record<string, boolean>,
	puts: [] as Record<string, boolean>[],
	failPut: false,
	failGet: false,
	hold: null as null | { release: () => void }
}));

vi.mock('$lib/api/client', () => ({
	api: {
		notificationSettings: {
			get: vi.fn(async () => {
				if (state.failGet) throw new Error('Load refused');
				return { ...state.server };
			}),
			put: vi.fn(async (body: Record<string, boolean>) => {
				state.puts.push(body);
				if (state.hold) await new Promise<void>((r) => (state.hold!.release = r));
				if (state.failPut) throw new Error('Save refused');
				state.server = { ...body };
				return { ...body };
			})
		}
	}
}));

const { default: NotificationSettings } = await import('./NotificationSettings.svelte');

beforeEach(() => {
	state.server = Object.fromEntries(KEYS.map((k) => [k, k !== 'include_agent_activity']));
	state.puts.length = 0;
	state.failPut = false;
	state.failGet = false;
	state.hold = null;
});

const box = (key: (typeof KEYS)[number]) =>
	screen.getByRole('checkbox', { name: LABELS[key] }) as HTMLInputElement;

async function shown() {
	render(NotificationSettings);
	await waitFor(() => expect(screen.getAllByRole('checkbox').length).toBeGreaterThan(0));
}

describe('NotificationSettings', () => {
	it('shows all seven toggles with the server values', async () => {
		await shown();
		expect(screen.getAllByRole('checkbox')).toHaveLength(7);
		for (const k of KEYS) expect(box(k).checked).toBe(k !== 'include_agent_activity');
		expect(screen.getByText(/never include item titles or comment text/)).toBeInTheDocument();
	});

	it.each(KEYS)('toggling %s sends that key flipped and the rest unchanged', async (key) => {
		await shown();
		const was = box(key).checked;
		await fireEvent.click(box(key));
		await waitFor(() => expect(state.puts).toHaveLength(1));
		const expected = Object.fromEntries(KEYS.map((k) => [k, k === key ? !was : k !== 'include_agent_activity']));
		expect(state.puts[0]).toEqual(expected);
		await waitFor(() => expect(box(key).checked).toBe(!was));
	});

	it('every PUT carries exactly the seven keys, all booleans', async () => {
		await shown();
		await fireEvent.click(box('notify_mentions'));
		await waitFor(() => expect(state.puts).toHaveLength(1));
		expect(Object.keys(state.puts[0]).sort()).toEqual([...KEYS].sort());
		expect(Object.values(state.puts[0]).every((v) => typeof v === 'boolean')).toBe(true);
	});

	it('a failed save reverts the toggle, says why, and is not wedged', async () => {
		await shown();
		state.failPut = true;
		await fireEvent.click(box('notify_assigned'));
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Save refused'));
		expect(box('notify_assigned').checked).toBe(true);
		state.failPut = false;
		await fireEvent.click(box('notify_assigned'));
		await waitFor(() => expect(box('notify_assigned').checked).toBe(false));
		expect(screen.queryByRole('alert')).toBeNull();
	});

	it('marks every toggle aria-disabled while a save is in flight, and a second toggle sends no PUT and snaps back', async () => {
		await shown();
		state.hold = { release: () => {} };
		await fireEvent.click(box('notify_assigned'));
		await waitFor(() => expect(box('notify_replies').getAttribute('aria-disabled')).toBe('true'));
		const stored = box('notify_replies').checked;
		await fireEvent.click(box('notify_replies'));
		expect(state.puts).toHaveLength(1);
		expect(box('notify_replies').checked).toBe(stored);
		state.hold.release();
		await waitFor(() => expect(box('notify_replies').getAttribute('aria-disabled')).toBe('false'));
	});

	it('keeps focus on the toggled input across a pending save', async () => {
		await shown();
		state.hold = { release: () => {} };
		box('notify_assigned').focus();
		await fireEvent.click(box('notify_assigned'));
		await waitFor(() => expect(box('notify_assigned').getAttribute('aria-disabled')).toBe('true'));
		expect(document.activeElement).toBe(box('notify_assigned'));
		state.hold.release();
		await waitFor(() => expect(box('notify_assigned').getAttribute('aria-disabled')).toBe('false'));
		expect(document.activeElement).toBe(box('notify_assigned'));
	});

	describe('#notifications deep link', () => {
		let section: HTMLElement;
		let spy: ReturnType<typeof vi.fn>;
		beforeEach(() => {
			section = document.createElement('section');
			section.id = 'notifications';
			spy = vi.fn();
			section.scrollIntoView = spy;
			document.body.appendChild(section);
		});
		afterEach(() => {
			section.remove();
			history.replaceState(null, '', location.pathname);
		});

		it('scrolls the section into view exactly once when the hash matches, not again on retry', async () => {
			history.replaceState(null, '', '#notifications');
			state.failGet = true;
			render(NotificationSettings);
			await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Load refused'));
			state.failGet = false;
			await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
			await waitFor(() => expect(screen.getAllByRole('checkbox')).toHaveLength(7));
			expect(spy).toHaveBeenCalledTimes(1);
		});

		it('never scrolls without the hash', async () => {
			history.replaceState(null, '', '#other');
			await shown();
			expect(spy).not.toHaveBeenCalled();
		});
	});

	it('a failed load shows the error and Retry, with no toggles; Retry loads', async () => {
		state.failGet = true;
		render(NotificationSettings);
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Load refused'));
		expect(screen.queryAllByRole('checkbox')).toHaveLength(0);
		state.failGet = false;
		await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
		await waitFor(() => expect(screen.getAllByRole('checkbox')).toHaveLength(7));
	});

	it('with push off, the other toggles stay editable and a note says nothing is sent', async () => {
		state.server.push_enabled = false;
		await shown();
		expect(screen.getByText('Push is off, so nothing is sent to your devices.')).toBeInTheDocument();
		for (const k of KEYS) expect(box(k).getAttribute('aria-disabled')).not.toBe('true');
	});

	it('with push on there is no off note', async () => {
		await shown();
		expect(screen.queryByText(/Push is off/)).toBeNull();
	});
});
