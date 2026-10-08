// TASK-2211 (audit C105): a tag page mixed completed items in with no way to
// leave them out. "Show completed" defaults ON, matching the collection list,
// is remembered per workspace like the view mode, and OFF asks the server for
// non-terminal items only.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';

const calls = vi.hoisted(() => [] as Array<Record<string, unknown> | undefined>);

vi.mock('$app/state', async () => ({ page: (await import('../../../../../test/mocks/reactivePage.svelte')).page }));
vi.mock('$app/environment', () => ({ browser: true }));
vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			list: vi.fn(async (_ws: string, params?: Record<string, unknown>) => {
				calls.push(params);
				return params?.non_terminal ? [] : [{ id: 'i1', title: 'Done thing', collection_id: 'c1', workspace_id: 'w1', fields: '{}', created_at: '', updated_at: '' }];
			})
		},
		collections: { list: vi.fn(async () => []) }
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { get current() { return { name: 'WS' }; } } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import TagPage from './+page.svelte';

beforeEach(() => {
	calls.length = 0;
	localStorage.clear();
	page.params = { username: 'dave', workspace: 'ws', tag: 'release' };
	page.url = new URL('http://localhost/dave/ws/tags/release');
});
afterEach(() => cleanup());

describe('TASK-2211: Show completed on a tag page', () => {
	it('defaults on, as the collection list does: no non_terminal filter', async () => {
		render(TagPage);
		await waitFor(() => expect(calls.length).toBeGreaterThan(0));
		expect(calls.at(-1)).toEqual({ tag: 'release' });
		expect((screen.getByLabelText('Show completed') as HTMLInputElement).checked).toBe(true);
	});

	it('off asks for open items only, says so when none are left, and is remembered', async () => {
		render(TagPage);
		await waitFor(() => expect(calls.length).toBeGreaterThan(0));
		(screen.getByLabelText('Show completed') as HTMLInputElement).click();
		await waitFor(() => expect(calls.at(-1)).toEqual({ tag: 'release', non_terminal: true }));
		await waitFor(() => expect(screen.getByText(/No open items tagged/)).toBeInTheDocument());
		// The switch stays reachable with nothing listed.
		expect(screen.getByLabelText('Show completed')).toBeInTheDocument();
		expect(localStorage.getItem('pad-tag-completed-ws')).toBe('hide');
	});

	it('a remembered "hide" applies on the next visit', async () => {
		localStorage.setItem('pad-tag-completed-ws', 'hide');
		render(TagPage);
		await waitFor(() => expect(calls.at(-1)).toEqual({ tag: 'release', non_terminal: true }));
	});
});
