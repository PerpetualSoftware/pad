import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';

type Feed = { id: string; layer: 'mine' | 'agent'; workspace_ids: string[]; name: string; created_at: string; last_used_at: string | null };
type CreateInput = { layer: Feed['layer']; workspace_ids: string[]; name?: string };
const state = vi.hoisted(() => ({
	feeds: [] as Feed[], creates: [] as CreateInput[], revokes: [] as string[], copied: [] as string[],
	createError: null as Error | null, failRevoke: false, failLoad: false, failWorkspaces: false, includeURL: true, copySucceeds: true
}));
const token = 'padics_TEST_ONLY_CALENDAR_SECRET';
const url = `https://calendar.example.test/api/v1/feeds/ics/${token}`;
const warning = 'This link is a secret. Your calendar provider (for example Google) stores it, and anyone who has it can see these titles. Revoke it if it leaks.';
const localNote = "This link uses this computer's address, so only calendar apps that can reach it (for example Calendar on this computer) can subscribe. Google Calendar and other online services can't reach it.";
const closeWarning = "You haven't copied this link. It won't be shown again.";
const limitReason = 'You have 10 calendar feeds, the most allowed. Revoke one to create another.';
vi.mock('$lib/api/client', () => ({ api: {
	calendarFeeds: {
		list: vi.fn(async () => {
			if (state.failLoad) throw new Error('Feeds unavailable');
			return state.feeds.map((feed) => ({ ...feed }));
		}),
		create: vi.fn(async (input: CreateInput) => {
			state.creates.push(input);
			if (state.createError) throw state.createError;
			return {
				feed: { id: 'created', name: '', created_at: '2026-11-12T12:00:00Z', last_used_at: null, ...input },
				token: 'padics_TEST_ONLY_CALENDAR_SECRET',
				...(state.includeURL ? { url: 'https://calendar.example.test/api/v1/feeds/ics/padics_TEST_ONLY_CALENDAR_SECRET' } : {})
			};
		}),
		revoke: vi.fn(async (id: string) => {
			state.revokes.push(id);
			if (state.failRevoke) throw new Error('Revoke refused');
		})
	},
	workspaces: { list: vi.fn(async () => {
		if (state.failWorkspaces) throw new Error('Workspaces unavailable');
		return [{ id: 'ws-design', name: 'Design' }, { id: 'ws-product', name: 'Product' }];
	}) }
} }));
vi.mock('$lib/utils/clipboard', () => ({ copyToClipboard: vi.fn(async (text: string) => { state.copied.push(text); return state.copySucceeds; }) }));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: vi.fn() } }));
const { default: CalendarFeedsSettings } = await import('./CalendarFeedsSettings.svelte');
function feed(overrides: Partial<Feed> = {}): Feed {
	return { id: 'f1', layer: 'mine', workspace_ids: [], name: 'Work dates', created_at: '2026-11-01T00:00:00Z', last_used_at: null, ...overrides };
}
beforeEach(() => {
	state.feeds = [feed()]; state.creates = []; state.revokes = []; state.copied = [];
	state.createError = null; state.failRevoke = false; state.failLoad = false; state.failWorkspaces = false; state.includeURL = true; state.copySucceeds = true;
});
async function shown() {
	render(CalendarFeedsSettings);
	await waitFor(() => expect(screen.getByRole('button', { name: 'New feed' })).toBeEnabled());
}
async function openCreate() {
	await shown();
	await fireEvent.click(screen.getByRole('button', { name: 'New feed' }));
	return screen.getByRole('dialog');
}
async function submitCreate() {
	await fireEvent.click(screen.getByRole('button', { name: 'Create feed' }));
	await waitFor(() => expect(screen.getByRole('button', { name: 'Copy' })).toBeInTheDocument());
}
describe('CalendarFeedsSettings', () => {
	it.each(['Done', 'Escape'])('reveals the URL only in the result and clears it on %s', async (close) => {
		const dialog = await openCreate();
		expect(document.body).not.toHaveTextContent(token);
		await fireEvent.input(screen.getByLabelText('Name (optional)'), { target: { value: 'Phone calendar' } });
		await submitCreate();
		expect(within(dialog).getByText(url)).toBeInTheDocument();
		expect(within(dialog).queryByText(localNote)).not.toBeInTheDocument();
		expect(screen.getByRole('list', { name: 'Calendar feeds' })).not.toHaveTextContent(token);
		await fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
		expect(state.copied).toEqual([url]);
		if (close === 'Done') await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
		else await fireEvent(dialog, new Event('cancel', { cancelable: true }));
		await waitFor(() => expect(document.body).not.toHaveTextContent(token));
		expect(screen.queryByText(closeWarning)).not.toBeInTheDocument();
		expect(screen.getByText('Phone calendar')).toBeInTheDocument();
		await fireEvent.click(screen.getByRole('button', { name: 'New feed' }));
		expect(document.body).not.toHaveTextContent(token);
	});
	it.each(['mine', 'agent'] as const)('shows the secret and one-time warnings beside the URL for %s', async (layer) => {
		const dialog = await openCreate();
		if (layer === 'agent') await fireEvent.click(screen.getByRole('radio', { name: 'Agent calendar' }));
		await submitCreate();
		expect(within(dialog).getByText(url)).toBeInTheDocument();
		expect(within(dialog).getByText(warning)).toBeInTheDocument();
		expect(within(dialog).getByText('This link is shown only once. Closing this dialog loses it.')).toBeInTheDocument();
		expect(state.creates[0]).toMatchObject({ layer, workspace_ids: [] });
	});
	it('defaults to My calendar and All workspaces, and sends the optional name', async () => {
		await openCreate();
		expect(screen.getByRole('radio', { name: 'My calendar' })).toBeChecked();
		expect(screen.getByRole('radio', { name: 'All workspaces' })).toBeChecked();
		await fireEvent.input(screen.getByLabelText('Name (optional)'), { target: { value: 'My phone' } });
		await submitCreate();
		expect(state.creates).toEqual([{ layer: 'mine', workspace_ids: [], name: 'My phone' }]);
	});
	it('requires a specific workspace selection and sends the selected layer and ids', async () => {
		await openCreate();
		await fireEvent.click(screen.getByRole('radio', { name: 'Agent calendar' }));
		await fireEvent.click(screen.getByRole('radio', { name: 'Specific workspaces' }));
		expect(screen.getByRole('button', { name: 'Create feed' })).toBeDisabled();
		await fireEvent.click(screen.getByRole('checkbox', { name: 'Design' }));
		await submitCreate();
		expect(state.creates[0]).toMatchObject({ layer: 'agent', workspace_ids: ['ws-design'] });
	});
	it('sends [] for All even after selecting a specific workspace', async () => {
		await openCreate();
		await fireEvent.click(screen.getByRole('radio', { name: 'Specific workspaces' }));
		await fireEvent.click(screen.getByRole('checkbox', { name: 'Product' }));
		await fireEvent.click(screen.getByRole('radio', { name: 'All workspaces' }));
		await submitCreate();
		expect(state.creates[0].workspace_ids).toEqual([]);
	});
	it('builds and copies a local URL when no public URL is returned, then clears it on close', async () => {
		state.includeURL = false;
		const dialog = await openCreate();
		await submitCreate();
		const localURL = `${window.location.origin}/api/v1/feeds/ics/${token}`;
		expect(within(dialog).getByText(localURL)).toBeInTheDocument();
		expect(within(dialog).queryByText(token, { exact: true })).not.toBeInTheDocument();
		expect(within(dialog).getByText(localNote)).toBeInTheDocument();
		expect(within(dialog).getByText(warning)).toBeInTheDocument();
		await fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
		expect(state.copied).toEqual([localURL]);
		await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
		expect(document.body).not.toHaveTextContent(token);
	});
	it.each(['Escape', 'Done', 'Close', 'backdrop'])('guards an uncopied secret on %s and supports keeping or discarding it', async (route) => {
		const dialog = await openCreate();
		await submitCreate();
		async function close() {
			if (route === 'Escape') await fireEvent(dialog, new Event('cancel', { cancelable: true }));
			else if (route === 'backdrop') {
				await fireEvent.pointerDown(dialog);
				await fireEvent.pointerUp(dialog);
				await fireEvent.click(dialog);
			} else await fireEvent.click(screen.getByRole('button', { name: route, exact: true }));
		}
		await close();
		expect(screen.getByRole('dialog')).toBe(dialog);
		expect(within(dialog).getByText(closeWarning)).toBeInTheDocument();
		expect(within(dialog).getByText(url)).toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Keep open' })).toHaveClass('primary');
		expect(screen.getByRole('button', { name: 'Close anyway' })).toHaveClass('secondary');
		await fireEvent.click(screen.getByRole('button', { name: 'Keep open' }));
		expect(screen.queryByText(closeWarning)).not.toBeInTheDocument();
		expect(within(dialog).getByText(url)).toBeInTheDocument();
		await close();
		await fireEvent.click(screen.getByRole('button', { name: 'Close anyway' }));
		expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
		expect(document.body).not.toHaveTextContent(token);
	});
	it('still guards the secret when copying fails', async () => {
		state.copySucceeds = false;
		await openCreate(); await submitCreate();
		await fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
		expect(screen.getByText(closeWarning)).toBeInTheDocument();
	});
	it('puts Cancel and the primary create submit in the same footer', async () => {
		const dialog = await openCreate();
		const create = screen.getByRole('button', { name: 'Create feed' });
		expect(create).toHaveClass('primary');
		const footer = dialog.querySelector('footer');
		expect(footer).not.toBeNull();
		expect(footer).toContainElement(create);
		expect(footer).toContainElement(screen.getByRole('button', { name: 'Cancel' }));
		expect(create).toHaveAttribute('type', 'submit');
		expect(create.getAttribute('form')).toBe(dialog.querySelector('form')!.id);
	});
	it('uses a solid danger action for the final revoke confirmation', async () => {
		await shown();
		await fireEvent.click(screen.getByRole('button', { name: 'Revoke Work dates' }));
		expect(screen.getByRole('button', { name: 'Revoke feed' })).toHaveClass('danger-solid');
		const dialog = screen.getByRole('dialog', { name: 'Revoke this feed?' });
		expect(dialog.querySelector('.subject')).toHaveTextContent('Work dates');
	});
	it('shows calendar, workspace names, unavailable counts, dates and a fallback name without raw ids', async () => {
		state.feeds = [feed(), feed({ id: 'f2', name: '', layer: 'agent', workspace_ids: ['ws-design', 'hidden-workspace-id'], last_used_at: '2026-11-02T00:00:00Z' })];
		await shown();
		const list = screen.getByRole('list', { name: 'Calendar feeds' });
		for (const text of ['My calendar', 'Agent calendar feed', 'All workspaces', 'Design', '1 unavailable workspace', 'Nov 1, 2026', 'Never', 'Nov 2, 2026']) expect(list).toHaveTextContent(text);
		expect(list).not.toHaveTextContent('hidden-workspace-id');
		expect(within(list).getAllByRole('button', { name: /Revoke/ })).toHaveLength(2);
	});
	it('asks before revoking, names the feed, and removes it only after confirmation', async () => {
		await shown();
		await fireEvent.click(screen.getByRole('button', { name: /Revoke/ }));
		expect(state.revokes).toEqual([]);
		expect(screen.getByRole('dialog')).toHaveTextContent('Work dates');
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
		expect(state.revokes).toEqual([]);
		await fireEvent.click(screen.getByRole('button', { name: /Revoke/ }));
		await fireEvent.click(screen.getByRole('button', { name: 'Revoke feed' }));
		await waitFor(() => expect(state.revokes).toEqual(['f1']));
		await waitFor(() => expect(screen.queryByText('Work dates')).toBeNull());
	});
	it('keeps the row and shows an error when revoke fails', async () => {
		state.failRevoke = true;
		await shown();
		await fireEvent.click(screen.getByRole('button', { name: /Revoke/ }));
		await fireEvent.click(screen.getByRole('button', { name: 'Revoke feed' }));
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Revoke refused'));
		expect(screen.getByRole('list', { name: 'Calendar feeds' })).toHaveTextContent('Work dates');
	});
	it.each([9, 10])('enforces the create limit with %i feeds', async (count) => {
		state.feeds = Array.from({ length: count }, (_, i) => feed({ id: `f${i}` }));
		render(CalendarFeedsSettings);
		await screen.findByRole('list', { name: 'Calendar feeds' });
		const create = screen.getByRole('button', { name: 'New feed' });
		if (count === 10) { expect(create).toBeDisabled(); expect(screen.getByText(limitReason)).toBeInTheDocument(); }
		else { expect(create).toBeEnabled(); expect(screen.queryByText(limitReason)).toBeNull(); }
	});
	it('shows the server range validation message on create', async () => {
		state.createError = Object.assign(new Error('At most 10 calendar feeds are allowed'), { status: 400, code: 'validation_error', details: { field: 'feeds', code: 'range' } });
		await openCreate();
		await fireEvent.click(screen.getByRole('button', { name: 'Create feed' }));
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('At most 10 calendar feeds are allowed'));
		expect(document.body).not.toHaveTextContent(token);
	});
	it.each(['failLoad', 'failWorkspaces'] as const)('shows an inline load error with retry for %s', async (failure) => {
		state[failure] = true;
		render(CalendarFeedsSettings);
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/unavailable/));
		expect(screen.getByRole('button', { name: 'New feed' })).toBeDisabled();
		state[failure] = false;
		await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
		await waitFor(() => expect(screen.getByRole('list', { name: 'Calendar feeds' })).toHaveTextContent('Work dates'));
	});
});
