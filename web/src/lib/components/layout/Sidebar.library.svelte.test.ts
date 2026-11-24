import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const ws = vi.hoisted(() => ({ current: { slug: 'ws', owner_username: 'u', is_guest: false } as { slug: string; owner_username: string; owner_id?: string; is_guest: boolean } }));

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []), update: vi.fn() },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn(), beforeNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get current() {
			return ws.current;
		},
		canEditCollection: () => true,
		membershipKnown: true,
	},
}));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));

import Sidebar from './Sidebar.svelte';

// TASK-2258 (audit C43): the sidebar's Agent section links the library, the
// discovery surface for invokable playbooks and conventions, for anyone who
// can activate from it; a guest does not get the link.
describe('Sidebar library link (TASK-2258)', () => {
	beforeEach(() => cleanup());

	it('a member sees Library in the Agent section', async () => {
		ws.current = { slug: 'ws', owner_username: 'u', is_guest: false };
		render(Sidebar);
		await tick();
		const link = document.querySelector<HTMLAnchorElement>('a.nav-item[href="/u/ws/library"]');
		expect(link?.textContent).toContain('Library');
		expect(document.querySelector('.section-header.agent-section')).not.toBeNull();
	});

	it('a guest does not', async () => {
		ws.current = { slug: 'ws', owner_username: 'u', is_guest: true };
		render(Sidebar);
		await tick();
		expect(document.querySelector('a[href="/u/ws/library"]')).toBeNull();
	});
});

// BUG-4802: owner_username can be ''. With no prefix to build, the workspace nav
// is not rendered, rather than linking `/library`, `/settings` and so on.
describe('Sidebar with no owner name', () => {
	beforeEach(() => cleanup());

	it('renders no workspace link from an empty prefix', async () => {
		ws.current = { slug: 'ws', owner_username: '', owner_id: 'u-other', is_guest: false };
		render(Sidebar);
		await tick();
		expect(document.querySelector('.collection-nav')).toBeNull();
		for (const path of ['/insights', '/graph', '/activity', '/library', '/settings']) {
			expect(document.querySelector(`a[href="${path}"]`), path).toBeNull();
		}
		expect(document.querySelector('a.settings-btn')).toBeNull();
	});
});
