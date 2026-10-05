import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import type { ItemAppAction } from '$lib/types';

// SPEC-6 U9d (TASK-3413) over U11: an installed app's item action is a link
// out. The tab opens inside the click (an open after an await is blocked as
// a popup), loses its opener, and only then goes to the minted URL.

const listMock = vi.fn<(ws: string, slug: string) => Promise<ItemAppAction[]>>();
const mintMock = vi.fn<(ws: string, slug: string, install: string, key: string) => Promise<{ url: string }>>();
const auth = { identityEpoch: 1 };

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			appActions: (ws: string, slug: string) => listMock(ws, slug),
			mintAppAction: (ws: string, slug: string, install: string, key: string) => mintMock(ws, slug, install, key)
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

const { default: ItemAppActions } = await import('./ItemAppActions.svelte');

const action: ItemAppAction = { install_id: 'inst-1', app_title: 'Support Portal', action_key: 'open-ticket', label: 'Open ticket' };

function fakeTab() {
	return { opener: {} as unknown, closed: false, location: { replace: vi.fn() }, close: vi.fn() };
}

let tab: ReturnType<typeof fakeTab>;
let openSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
	vi.clearAllMocks();
	auth.identityEpoch = 1;
	tab = fakeTab();
	openSpy = vi.spyOn(window, 'open').mockImplementation(() => tab as unknown as Window);
});

describe('ItemAppActions', () => {
	it('renders nothing when no action applies', async () => {
		listMock.mockResolvedValue([]);
		const { container } = render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await waitFor(() => expect(listMock).toHaveBeenCalledWith('ws', 'TASK-1'));
		expect(container.querySelector('button')).toBeNull();
	});

	it('opens the tab inside the click, drops its opener, then sends it to the minted URL', async () => {
		listMock.mockResolvedValue([action]);
		let resolve: (v: { url: string }) => void = () => {};
		mintMock.mockReturnValue(new Promise((r) => (resolve = r)));
		render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await fireEvent.click(await screen.findByRole('button', { name: /Open ticket/ }));
		// Before the mint has answered: the tab exists and has no opener.
		expect(openSpy).toHaveBeenCalledTimes(1);
		expect(openSpy.mock.calls[0][0]).toBe('');
		expect(tab.opener).toBeNull();
		expect(tab.location.replace).not.toHaveBeenCalled();
		resolve({ url: 'https://portal.example/ctx?code=abc' });
		await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith('https://portal.example/ctx?code=abc'));
		expect(mintMock).toHaveBeenCalledWith('ws', 'TASK-1', 'inst-1', 'open-ticket');
	});

	it('never follows a non-web URL', async () => {
		listMock.mockResolvedValue([action]);
		mintMock.mockResolvedValue({ url: 'javascript:alert(1)' });
		render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await fireEvent.click(await screen.findByRole('button', { name: /Open ticket/ }));
		await waitFor(() => expect(tab.close).toHaveBeenCalled());
		expect(tab.location.replace).not.toHaveBeenCalled();
		expect((await screen.findByRole('alert')).textContent).toBe("Couldn't open Support Portal.");
	});

	it('a refused mint closes the tab and says only that it did not open', async () => {
		listMock.mockResolvedValue([action]);
		mintMock.mockRejectedValue(new Error('Not found'));
		render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await fireEvent.click(await screen.findByRole('button', { name: /Open ticket/ }));
		await waitFor(() => expect(tab.close).toHaveBeenCalled());
		expect((await screen.findByRole('alert')).textContent).toBe("Couldn't open Support Portal.");
	});

	it('a sign-out during the mint closes the tab and opens nothing', async () => {
		listMock.mockResolvedValue([action]);
		let resolve: (v: { url: string }) => void = () => {};
		mintMock.mockReturnValue(new Promise((r) => (resolve = r)));
		render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await fireEvent.click(await screen.findByRole('button', { name: /Open ticket/ }));
		auth.identityEpoch = 2;
		resolve({ url: 'https://portal.example/ctx?code=abc' });
		await waitFor(() => expect(tab.close).toHaveBeenCalled());
		expect(tab.location.replace).not.toHaveBeenCalled();
	});

	it('an item switch during the mint (the parent remounts) closes the tab and opens nothing', async () => {
		listMock.mockResolvedValue([action]);
		let resolve: (v: { url: string }) => void = () => {};
		mintMock.mockReturnValue(new Promise((r) => (resolve = r)));
		const { unmount } = render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await fireEvent.click(await screen.findByRole('button', { name: /Open ticket/ }));
		unmount();
		resolve({ url: 'https://portal.example/ctx?code=abc' });
		await waitFor(() => expect(tab.close).toHaveBeenCalled());
		expect(tab.location.replace).not.toHaveBeenCalled();
	});

	it('with popups blocked outright, falls back to a no-opener open of the URL', async () => {
		listMock.mockResolvedValue([action]);
		mintMock.mockResolvedValue({ url: 'https://portal.example/ctx?code=abc' });
		openSpy.mockImplementationOnce(() => null);
		render(ItemAppActions, { wsSlug: 'ws', itemSlug: 'TASK-1' });
		await fireEvent.click(await screen.findByRole('button', { name: /Open ticket/ }));
		await waitFor(() => expect(openSpy).toHaveBeenCalledTimes(2));
		expect(openSpy.mock.calls[1]).toEqual(['https://portal.example/ctx?code=abc', '_blank', 'noopener,noreferrer']);
	});
});
