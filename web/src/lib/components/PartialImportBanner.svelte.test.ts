// TASK-896: the banner for a workspace an import kept partly built. Owners
// see the server's (sanitized) reason and can keep the workspace, which
// clears the marker; everyone else sees only that it is partly imported.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	gets: [] as string[],
	updates: [] as Array<{ slug: string; data: Record<string, unknown> }>,
	setCurrent: [] as unknown[],
	current: null as { slug: string } | null,
	epoch: 0
}));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn((slug: string) => {
				mocks.gets.push(slug);
				return Promise.resolve({ slug, import_status: { status: 'partial', note: 'Reference imp-abc.' } });
			}),
			update: vi.fn((slug: string, data: Record<string, unknown>) => {
				mocks.updates.push({ slug, data });
				return Promise.resolve({ slug });
			})
		}
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get current() {
			return mocks.current;
		},
		setCurrent: vi.fn((ws: unknown) => {
			mocks.setCurrent.push(ws);
			return Promise.resolve();
		}),
		loadAll: vi.fn(() => Promise.resolve())
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() {
			return mocks.epoch;
		}
	}
}));

import PartialImportBanner from './PartialImportBanner.svelte';

async function flush() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}
const banner = () => document.querySelector('[data-testid="partial-import-banner"]');
const partial = { status: 'partial' as const };

beforeEach(() => {
	mocks.gets = [];
	mocks.updates = [];
	mocks.setCurrent = [];
	mocks.current = { slug: 'ws' };
	mocks.epoch = 0;
});
afterEach(() => cleanup());

describe('PartialImportBanner', () => {
	it('renders nothing without a marker', async () => {
		render(PartialImportBanner, { slug: 'ws', status: undefined, isOwner: true });
		await flush();
		expect(banner()).toBeNull();
		expect(mocks.gets).toEqual([]);
	});

	it('shows a non-owner only that the workspace is partly imported', async () => {
		render(PartialImportBanner, { slug: 'ws', status: partial, isOwner: false });
		await flush();
		expect(banner()?.textContent).toContain('only partly imported');
		expect(document.querySelector('[data-testid="partial-import-note"]')).toBeNull();
		expect(document.querySelector('.pi-keep')).toBeNull();
		expect(mocks.gets).toEqual([]);
	});

	it('shows an owner the reason and clears the marker on Keep it', async () => {
		render(PartialImportBanner, {
			slug: 'ws',
			status: partial,
			isOwner: true,
			deleteHref: '/u/ws/settings#danger'
		});
		await flush();
		expect(mocks.gets).toEqual(['ws']);
		expect(document.querySelector('[data-testid="partial-import-note"]')?.textContent).toBe('Reference imp-abc.');
		expect(document.querySelector('a.pi-delete')?.getAttribute('href')).toBe('/u/ws/settings#danger');

		await fireEvent.click(document.querySelector('.pi-keep')!);
		await flush();
		expect(mocks.updates).toEqual([{ slug: 'ws', data: { clear_import_status: true } }]);
		expect(mocks.setCurrent).toEqual([{ slug: 'ws' }]);
		expect(banner()).toBeNull();
	});

	it('commits nothing to the store when the identity changed under the PATCH', async () => {
		render(PartialImportBanner, { slug: 'ws', status: partial, isOwner: true });
		await flush();
		const click = fireEvent.click(document.querySelector('.pi-keep')!);
		mocks.epoch = 1;
		await click;
		await flush();
		expect(mocks.updates).toHaveLength(1);
		expect(mocks.setCurrent).toEqual([]);
	});

	it('offers the settings page an in-page delete', async () => {
		const ondelete = vi.fn();
		render(PartialImportBanner, { slug: 'ws', status: partial, isOwner: true, ondelete });
		await flush();
		await fireEvent.click(document.querySelector('button.pi-delete')!);
		expect(ondelete).toHaveBeenCalledOnce();
	});
});
