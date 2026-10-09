import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';

/**
 * TASK-2256 (audit C66 + C67) on the Library page:
 *  - C66: the tab lives in the URL (a refresh keeps it, and the per-tab
 *    scroll keys finally differ), and the tabs follow the tabs pattern.
 *  - C67: a card can show its full text before Activate, and "Active" links
 *    to the item activation created.
 */
const auth = vi.hoisted(() => ({
	get identityEpoch() { return 0; },
	get userId() { return 'u1'; },
	get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
	identityFence() { return () => true; },
	onIdentityChange() { return () => {}; },
	clear() {}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));
vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));

const nav = vi.hoisted(() => ({ calls: [] as { url: string; opts: unknown }[] }));
vi.mock('$app/navigation', async () => {
	const { page } = await import('../../../../test/mocks/reactivePage.svelte');
	return {
		goto: vi.fn(async (url: string | URL, opts?: unknown) => {
			nav.calls.push({ url: String(url), opts });
			page.url = new URL(String(url), page.url);
		})
	};
});

const LONG = 'Write every commit message in the conventional format. ' + 'The rest of the rule, which the card used to cut off. '.repeat(3) + 'THE-LAST-SENTENCE.';
const data = vi.hoisted(() => ({
	conventions: [] as unknown[],
	builtins: [] as unknown[],
	created: null as unknown
}));

vi.mock('$lib/api/client', () => ({
	api: {
		library: {
			get: vi.fn(async () => ({
				categories: [
					{
						name: 'git',
						conventions: [
							{ key: 'convention/commits', title: 'Use conventional commits', content: LONG, category: 'git', trigger: 'on-commit', enforcement: 'should', surfaces: ['all'] },
							{ title: 'Short rule', content: 'Short.', category: 'git', trigger: 'always', enforcement: 'must', surfaces: ['all'] }
						]
					}
				]
			})),
			getPlaybooks: vi.fn(async () => ({
				categories: [{ name: 'agent-workflows', playbooks: [{ key: 'playbook/ship', title: 'Ship', content: '1. one\n2. two', category: 'agent-workflows', trigger: 'manual', scope: 'all', invocation_slug: 'ship', arguments: [{ name: 'target', type: 'ref' }] }] }]
			})),
			activate: vi.fn(async () => data.created),
			activatePlaybook: vi.fn(async () => data.created)
		},
		items: {
			listByCollection: vi.fn(async (_ws: string, slug: string) => (slug === 'conventions' ? data.conventions : []))
		},
		builtins: { list: vi.fn(async () => data.builtins) },
		collections: { list: vi.fn(async () => []) }
	}
}));
vi.mock('$lib/collections/canCreateIn', () => ({ canCreateIn: () => true }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '../../../../test/mocks/reactivePage.svelte';
import LibraryPage from './+page.svelte';

afterEach(() => {
	cleanup();
	nav.calls.length = 0;
	data.conventions = [];
	data.builtins = [];
	data.created = null;
});

async function mount(search = '') {
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL(`http://localhost/dave/ws/library${search}`);
	render(LibraryPage);
	await screen.findByRole('tablist', { name: 'Library' });
}

describe('C66: the library tab is in the URL and is a real tablist', () => {
	it('opens on the tab the URL names', async () => {
		await mount('?tab=playbooks');
		expect(screen.getByRole('tab', { name: 'Playbooks' }).getAttribute('aria-selected')).toBe('true');
		expect(await screen.findByText('Ship')).toBeTruthy();
		expect(screen.queryByText('Use conventional commits')).toBeNull();
	});

	it('a click writes ?tab= with replaceState, and the panel follows', async () => {
		await mount();
		await screen.findByText('Use conventional commits');
		await fireEvent.click(screen.getByRole('tab', { name: 'Playbooks' }));
		expect(nav.calls).toHaveLength(1);
		expect(new URL(nav.calls[0].url).searchParams.get('tab')).toBe('playbooks');
		expect(nav.calls[0].opts).toMatchObject({ replaceState: true });
		await screen.findByText('Ship');
		const panel = screen.getByRole('tabpanel');
		expect(panel.getAttribute('aria-labelledby')).toBe('library-tab-playbooks');
	});

	it('roving tabindex, and the arrow keys move selection and focus', async () => {
		await mount();
		const conv = screen.getByRole('tab', { name: 'Conventions' });
		const pb = screen.getByRole('tab', { name: 'Playbooks' });
		expect(conv.getAttribute('tabindex')).toBe('0');
		expect(pb.getAttribute('tabindex')).toBe('-1');
		expect(conv.getAttribute('aria-controls')).toBe('library-tabpanel');
		conv.focus();
		await fireEvent.keyDown(conv, { key: 'ArrowRight' });
		await waitFor(() => expect(pb.getAttribute('aria-selected')).toBe('true'));
		expect(document.activeElement).toBe(pb);
		await fireEvent.keyDown(pb, { key: 'Home' });
		await waitFor(() => expect(conv.getAttribute('aria-selected')).toBe('true'));
		expect(document.activeElement).toBe(conv);
	});
});

describe('C67: read before Activate, and Active leads to the item', () => {
	it('a long card shows its full text on request; a short one offers nothing', async () => {
		await mount();
		await screen.findByText('Use conventional commits');
		expect(screen.queryByText(/THE-LAST-SENTENCE/)).toBeNull();
		const buttons = screen.getAllByRole('button', { name: 'Show full text' });
		expect(buttons).toHaveLength(1);
		expect(buttons[0].getAttribute('aria-expanded')).toBe('false');
		await fireEvent.click(buttons[0]);
		expect(screen.getByText(/THE-LAST-SENTENCE/)).toBeTruthy();
		const less = screen.getByRole('button', { name: 'Show less' });
		expect(less.getAttribute('aria-expanded')).toBe('true');
		expect(document.getElementById(less.getAttribute('aria-controls')!)).toBeTruthy();
	});

	it('Active links to the item made from the entry, by key', async () => {
		data.builtins = [{ item_id: 'i1', ref: 'CONVE-7', slug: 'use-conventional-commits', title: 'Renamed', collection_slug: 'conventions', key: 'convention/commits', state: 'current' }];
		await mount();
		const link = await screen.findByRole('link', { name: /Active: open Use conventional commits/ });
		expect(link.getAttribute('href')).toBe('/dave/ws/conventions/CONVE-7');
	});

	it('Active links by title when the item has no recorded origin', async () => {
		data.conventions = [{ id: 'i2', title: 'Short rule', slug: 'short-rule', collection_slug: 'conventions', item_number: 9, collection_prefix: 'CONVE' }];
		await mount();
		const link = await screen.findByRole('link', { name: /Active: open Short rule/ });
		expect(link.getAttribute('href')).toMatch(/^\/dave\/ws\/conventions\//);
	});

	it('an entry activated on this page links to the item the server created', async () => {
		data.created = { id: 'i3', title: 'Short rule', slug: 'short-rule', collection_slug: 'conventions', item_number: 12, collection_prefix: 'CONVE' };
		await mount();
		await screen.findByText('Short rule');
		const buttons = screen.getAllByRole('button', { name: 'Activate' });
		await fireEvent.click(buttons[1]);
		const link = await screen.findByRole('link', { name: /Active: open Short rule/ });
		expect(link.getAttribute('href')).toMatch(/^\/dave\/ws\/conventions\//);
	});
});
