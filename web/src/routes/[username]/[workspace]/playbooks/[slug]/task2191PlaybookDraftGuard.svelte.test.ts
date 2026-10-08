import { describe, expect, it, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';

/**
 * TASK-2191: the playbook editor discarded a draft on any way out (Cancel,
 * the back link, a reload) without asking, and Save always went back to the
 * list, so every iteration meant re-opening the playbook. Now every way out
 * asks when there are unsaved changes, Save stays on the page, and Save and
 * close re-baselines BEFORE it navigates, so it never asks about what it has
 * just stored.
 */
type Nav = { type: string; cancel: () => void };
const nav = vi.hoisted(() => ({ guard: null as null | ((n: Nav) => void), gotos: [] as string[] }));
vi.mock('$app/navigation', () => ({
	beforeNavigate: (fn: (n: Nav) => void) => {
		nav.guard = fn;
	},
	// A navigation runs the guard first, as SvelteKit does.
	goto: (url: string) => {
		let cancelled = false;
		nav.guard?.({ type: 'goto', cancel: () => (cancelled = true) });
		if (!cancelled) nav.gotos.push(url);
		return Promise.resolve();
	},
	afterNavigate: () => {},
	invalidate: () => Promise.resolve(),
	invalidateAll: () => Promise.resolve()
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; }
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => true } }));
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: () => 'id', dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false
}));

const PLAYBOOK = {
	id: 'p1',
	slug: 'ship-it',
	title: 'Ship it',
	content: 'body',
	collection_slug: 'playbooks',
	collection_prefix: 'PLAYB',
	item_number: 7,
	seq: 3,
	fields: JSON.stringify({ status: 'active', trigger: 'manual', scope: 'all', invocation_slug: 'ship' })
};
const updates = vi.hoisted(() => [] as unknown[]);
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: {
		items: {
			get: vi.fn(async () => PLAYBOOK),
			listByCollection: vi.fn(async () => []),
			update: vi.fn(async (_ws: string, _slug: string, payload: Record<string, unknown>) => {
				updates.push(payload);
				return { ...PLAYBOOK, title: payload.title, seq: PLAYBOOK.seq + updates.length };
			})
		},
		collections: {
			list: vi.fn(async () => []),
			get: vi.fn(async () => ({ id: 'c', slug: 'playbooks', schema: '{"fields":[]}' }))
		}
	}
}));

import { page } from '$app/state';
import PlaybookEditor from './+page.svelte';

beforeEach(() => {
	nav.guard = null;
	nav.gotos.length = 0;
	updates.length = 0;
	page.params = { username: 'dave', workspace: 'ws', slug: 'PLAYB-7' };
	page.url = new URL('http://localhost/dave/ws/playbooks/PLAYB-7');
});
afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

async function openAndEdit(): Promise<HTMLInputElement> {
	render(PlaybookEditor);
	const title = (await screen.findByPlaceholderText('Playbook title')) as HTMLInputElement;
	title.value = 'Ship it, edited';
	title.dispatchEvent(new Event('input', { bubbles: true }));
	return title;
}

const leave = (type = 'link') => {
	let cancelled = false;
	nav.guard!({ type, cancel: () => (cancelled = true) });
	return cancelled;
};
const button = (re: RegExp) => screen.getAllByRole('button').find((b) => re.test(b.textContent ?? ''))!;

describe('TASK-2191: the playbook editor keeps a draft', () => {
	it('leaving an untouched playbook does not ask', async () => {
		render(PlaybookEditor);
		await screen.findByPlaceholderText('Playbook title');
		const ask = vi.spyOn(window, 'confirm');
		expect(leave()).toBe(false);
		expect(ask).not.toHaveBeenCalled();
	});

	it('leaving with unsaved changes asks, and a No stays', async () => {
		await openAndEdit();
		const ask = vi.spyOn(window, 'confirm').mockReturnValue(false);
		expect(leave()).toBe(true);
		expect(ask).toHaveBeenCalledTimes(1);
		ask.mockReturnValue(true);
		expect(leave()).toBe(false);
	});

	it('closing the tab with unsaved changes raises the browser prompt, not a dialog', async () => {
		await openAndEdit();
		const ask = vi.spyOn(window, 'confirm');
		expect(leave('leave')).toBe(true);
		expect(ask).not.toHaveBeenCalled();
	});

	it('Cancel with unsaved changes asks before going', async () => {
		await openAndEdit();
		vi.spyOn(window, 'confirm').mockReturnValue(false);
		button(/^cancel$/i).click();
		expect(nav.gotos).toEqual([]);
	});

	it('Save stays on the page, and leaving afterwards does not ask', async () => {
		await openAndEdit();
		button(/^save$/i).click();
		await waitFor(() => expect(updates).toHaveLength(1));
		await waitFor(() => expect(button(/^save$/i).textContent).toMatch(/^\s*save\s*$/i));
		expect(nav.gotos).toEqual([]);
		const ask = vi.spyOn(window, 'confirm');
		expect(leave()).toBe(false);
		expect(ask).not.toHaveBeenCalled();
	});

	it('Save and close goes back to the list without asking about what it just saved', async () => {
		await openAndEdit();
		const ask = vi.spyOn(window, 'confirm').mockReturnValue(false);
		button(/save and close/i).click();
		await waitFor(() => expect(nav.gotos).toEqual(['/dave/ws/playbooks']));
		expect(ask).not.toHaveBeenCalled();
	});
});
