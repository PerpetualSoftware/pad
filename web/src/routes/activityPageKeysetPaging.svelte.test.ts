import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import type { Activity } from '$lib/types';

/**
 * BUG-2781, the workspace activity page's wiring (CONVE-19): "Load more" asks
 * for the rows after the last one held, by keyset, and a row the server
 * returns twice renders once (the feed is a keyed each).
 */
const listActivity = vi.hoisted(() =>
	vi.fn<(slug: string, params: Record<string, unknown>) => Promise<Activity[]>>()
);

vi.mock('$lib/api/client', () => ({
	api: {
		collections: { list: vi.fn().mockResolvedValue([]) },
		activity: {
			list: (slug: string, params: Record<string, unknown>) => listActivity(slug, params)
		},
		comments: { list: vi.fn().mockResolvedValue([]) }
	},
	PadApiError: class extends Error {}
}));

const { default: ActivityPage } = await import('./[username]/[workspace]/activity/+page.svelte');

function act(i: number): Activity {
	return {
		id: `a${String(i).padStart(3, '0')}`,
		workspace_id: 'ws',
		action: 'settings_changed',
		actor: 'user',
		source: 'web',
		metadata: '{}',
		created_at: new Date(Date.UTC(2026, 7, 24, 12, 0, 0) - i * 1000).toISOString()
	} as Activity;
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle(): Promise<void> {
	flushSync();
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	page.params.workspace = 'ws';
	page.params.username = 'alice';
	localStorage.clear();
	listActivity.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('workspace activity paging', () => {
	it('pages by the last row held and renders a repeated row once', async () => {
		const page1 = Array.from({ length: 30 }, (_, i) => act(i));
		const page2 = [act(29), act(30)];
		listActivity.mockImplementation(async () => (listActivity.mock.calls.length === 1 ? page1 : page2));

		localStorage.setItem('pad-activity-view', 'audit');
		app = mount(ActivityPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();

		const btn = [...host.querySelectorAll('button')].find((b) => /load more/i.test(b.textContent ?? ''));
		expect(btn, 'a Load more button is rendered').toBeTruthy();
		btn!.click();
		await settle();

		expect(listActivity).toHaveBeenCalledTimes(2);
		const params = listActivity.mock.calls[1][1];
		expect(params.before).toBe(act(29).created_at);
		expect(params.before_id).toBe(act(29).id);
		expect('offset' in params).toBe(false);
		expect(host.querySelectorAll('.activity-entry, .date-entries > *')).toHaveLength(31);
	});
});
