// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4315: the root layout owns the chat unread store's lifecycle and the
// Chat setting gates it. With Chat off, signed out or the setting still unknown,
// NO unread request is made, not even by an access-stream resync (which reaches
// a never-started store). Turning Chat on fetches exactly once; off stops it.
// The chatStore here is the REAL one; only the network edge is a spy.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn(), setTimezoneIfUnset: vi.fn(async () => ({ timezone: 'UTC' })) },
	workspaces: { list: vi.fn() },
	chat: { unread: vi.fn() },
}));

vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy(api, {
			get: (t, k) => (k in t ? (t as never)[k] : new Proxy({}, { get: () => never })),
		}),
		setAccessRevokedHandler: () => {},
		setIdentityProvider: () => {},
		setRateLimitHandler: () => {},
		setDatabaseBusyHandler: () => {},
		setNetworkUnreachableHandler: () => {},
		isPlanLimitError: () => false,
		planLimitMessage: () => '',
	};
});
vi.mock('$lib/stores/identityReload.svelte', () => ({
	reloadForIdentityChange: vi.fn(),
	clearPersistentIdentityState: vi.fn(),
}));
// jsdom has no EventSource; the stream's own behaviour is pinned elsewhere. What
// it does to the unread store on a resync is `chatStore.resync()`, called below.
vi.mock('$lib/services/accessStream.svelte', () => ({ accessStream: { start: vi.fn() } }));
vi.mock('$app/navigation', () => ({
	goto: vi.fn(),
	beforeNavigate: () => {},
	afterNavigate: () => {},
	invalidateAll: vi.fn(),
	pushState: vi.fn(),
	replaceState: vi.fn(),
}));

import Layout from './+layout.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { chatStore } from '$lib/stores/chat.svelte';
import { page } from '$app/state';

const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

function session(chatEnabled?: boolean) {
	return {
		authenticated: true,
		user: { id: 'u1', email: 'u1@example.com', ...(chatEnabled === undefined ? {} : { chat_enabled: chatEnabled }) },
	};
}
async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}
// What the access stream does on a reconnect or sync_required.
function deliverResync() {
	chatStore.resync();
}

beforeEach(() => {
	vi.resetAllMocks();
	page.url = new URL('http://localhost/alice/ws');
	api.workspaces.list.mockResolvedValue([]);
	api.chat.unread.mockResolvedValue({ conversations: [], total: 0 });
});
afterEach(() => {
	cleanup();
	authStore.clear();
	chatStore.stop();
});

describe('TASK-4315: the root layout gates the chat unread store on the Chat setting', () => {
	it('Chat off: no unread request, including after a resync is delivered', async () => {
		api.auth.session.mockResolvedValue(session(false));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		deliverResync();
		await settle();
		window.dispatchEvent(new Event('focus'));
		await settle();
		expect(api.chat.unread).not.toHaveBeenCalled();
	});

	it('setting unknown (field absent): no unread request, including after a resync', async () => {
		api.auth.session.mockResolvedValue(session(undefined));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		deliverResync();
		await settle();
		expect(api.chat.unread).not.toHaveBeenCalled();
	});

	it('setting unknown (session not loaded yet): no unread request, including after a resync', async () => {
		api.auth.session.mockReturnValue(new Promise(() => {}));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		deliverResync();
		await settle();
		expect(api.chat.unread).not.toHaveBeenCalled();
	});

	it('Chat turns on: exactly one unread request', async () => {
		api.auth.session.mockResolvedValue(session(false));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(api.chat.unread).not.toHaveBeenCalled();

		authStore.setChatEnabled('u1', true);
		await settle();
		expect(api.chat.unread).toHaveBeenCalledTimes(1);
		// Nothing else in the layout re-runs the gate into a second fetch.
		await settle();
		expect(api.chat.unread).toHaveBeenCalledTimes(1);
	});

	it('Chat on at load: exactly one unread request', async () => {
		api.auth.session.mockResolvedValue(session(true));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(api.chat.unread).toHaveBeenCalledTimes(1);
	});

	it('Chat turned off again: stops, and a resync or focus makes no further request', async () => {
		api.auth.session.mockResolvedValue(session(true));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(api.chat.unread).toHaveBeenCalledTimes(1);

		authStore.setChatEnabled('u1', false);
		await settle();
		deliverResync();
		window.dispatchEvent(new Event('focus'));
		await settle();
		expect(api.chat.unread).toHaveBeenCalledTimes(1);
	});

	it('Chat off then on again re-starts the store', async () => {
		api.auth.session.mockResolvedValue(session(true));
		render(Layout, { props: { children: childSnippet } });
		await settle();
		authStore.setChatEnabled('u1', false);
		await settle();
		authStore.setChatEnabled('u1', true);
		await settle();
		expect(api.chat.unread).toHaveBeenCalledTimes(2);
	});

	it('signed out: no unread request, including after a resync', async () => {
		api.auth.session.mockResolvedValue({ authenticated: false });
		render(Layout, { props: { children: childSnippet } });
		await settle();
		deliverResync();
		await settle();
		expect(api.chat.unread).not.toHaveBeenCalled();
	});
});
