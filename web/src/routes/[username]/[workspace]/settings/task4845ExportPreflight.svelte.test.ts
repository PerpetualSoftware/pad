import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, cleanup } from '@testing-library/svelte';
import { page } from '$app/state';
import { tick } from 'svelte';
import SettingsPage from './+page.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';

/**
 * TASK-4845 — the workspace bundle export is preflighted so a refusal
 * (422 export_too_large) shows in the page instead of as a failed download,
 * and "Leave out version history" adds include_versions=false. Drives the
 * real page against a mocked client.
 */

const h = vi.hoisted(() => ({
	preflight: null as unknown as ReturnType<typeof import('vitest').vi.fn>
}));

vi.mock('$lib/api/client', () => {
	h.preflight = vi.fn(async () => undefined);
	return {
		api: {
			workspaces: {
				get: vi.fn(async () => ({
					id: 'ws1', slug: page.params.workspace, name: 'WS', context: {},
					created_at: '2026-10-01T00:00:00Z'
				})),
				me: vi.fn(async () => ({
					role: 'owner', collection_access: 'all', collection_grants: [], item_grants: []
				})),
				list: vi.fn(async () => []),
				exportUrl: (slug: string, opts: { includeVersions?: boolean } = {}) =>
					`/api/v1/workspaces/${slug}/export?format=tar${opts.includeVersions === false ? '&include_versions=false' : ''}`,
				preflightExport: (...args: unknown[]) => h.preflight(...args)
			},
			collections: { list: vi.fn(async () => []) },
			members: { list: vi.fn(async () => ({ members: [], invitations: [] })) }
		},
		isPlanLimitError: () => false,
		planLimitMessage: () => '',
		PadApiError: class extends Error {}
	};
});

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} }
}));

async function renderOwner(slug: string) {
	page.params = { username: 'dave', workspace: slug };
	render(SettingsPage);
	await screen.findByDisplayValue('WS');
	await waitFor(() => expect(workspaceStore.currentRole).toBe('owner'));
	await tick();
	return await screen.findByRole('button', { name: /Download \.tar\.gz/ });
}

let clicked: string[] = [];

beforeEach(() => {
	window.location.hash = '';
	clicked = [];
	h.preflight.mockReset();
	vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
		clicked.push(`${this.getAttribute('href')} ${this.getAttribute('download')}`);
	});
});

afterEach(() => {
	vi.restoreAllMocks();
});

describe('TASK-4845: workspace export preflight', () => {
	it('keeps history by default and starts the browser download on success', async () => {
		h.preflight.mockResolvedValue(undefined);
		const btn = await renderOwner('ws-ok');
		const box = screen.getByRole('checkbox', { name: /Leave out version history/ }) as HTMLInputElement;
		expect(box.checked).toBe(false);
		await fireEvent.click(btn);
		await waitFor(() => expect(clicked).toHaveLength(1));
		expect(h.preflight).toHaveBeenCalledWith('ws-ok', { includeVersions: true }, expect.any(AbortSignal));
		expect(clicked[0]).toBe('/api/v1/workspaces/ws-ok/export?format=tar ws-ok-export.tar.gz');
		expect(screen.queryByRole('alert')).toBeNull();
	});

	it('leaves out history when ticked', async () => {
		h.preflight.mockResolvedValue(undefined);
		const btn = await renderOwner('ws-nov');
		await fireEvent.click(screen.getByRole('checkbox', { name: /Leave out version history/ }));
		await fireEvent.click(btn);
		await waitFor(() => expect(clicked).toHaveLength(1));
		expect(h.preflight).toHaveBeenCalledWith('ws-nov', { includeVersions: false }, expect.any(AbortSignal));
		expect(clicked[0]).toBe('/api/v1/workspaces/ws-nov/export?format=tar&include_versions=false ws-nov-export.tar.gz');
	});

	it('shows export_too_large in the page, downloads nothing, and clears on toggle', async () => {
		h.preflight.mockRejectedValue(Object.assign(new Error('pad workspace export --no-versions'), {
			code: 'export_too_large',
			details: { size_reached: 70 * 1024 * 1024, limit_bytes: 64 * 1024 * 1024, include_versions: true }
		}));
		const btn = await renderOwner('ws-big');
		await fireEvent.click(btn);
		const alert = await screen.findByRole('alert');
		expect(alert.textContent).toContain('64 MiB');
		expect(alert.textContent).toContain('Leave out version history');
		expect(alert.textContent).not.toContain('--no-versions');
		expect(clicked).toHaveLength(0);
		expect((btn as HTMLButtonElement).disabled).toBe(false);

		await fireEvent.click(screen.getByRole('checkbox', { name: /Leave out version history/ }));
		await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
	});

	it('cancels a stalled preflight: Download comes back, no alert, no download', async () => {
		let seen: AbortSignal | undefined;
		h.preflight.mockImplementation(
			(_slug: string, _opts: unknown, signal?: AbortSignal) =>
				new Promise((_resolve, reject) => {
					seen = signal;
					signal?.addEventListener('abort', () =>
						reject(Object.assign(new Error('Export cancelled.'), { code: 'export_cancelled' }))
					);
				})
		);
		const btn = await renderOwner('ws-stall');
		await fireEvent.click(btn);
		await screen.findByRole('button', { name: /Preparing export/ });
		const cancel = await screen.findByRole('button', { name: /^Cancel$/ });
		expect(seen).toBeInstanceOf(AbortSignal);
		await fireEvent.click(cancel);
		expect(seen?.aborted).toBe(true);
		const again = await screen.findByRole('button', { name: /Download \.tar\.gz/ });
		expect((again as HTMLButtonElement).disabled).toBe(false);
		expect(screen.queryByRole('button', { name: /^Cancel$/ })).toBeNull();
		await tick();
		await new Promise((r) => setTimeout(r, 0));
		expect(screen.queryByRole('alert')).toBeNull();
		expect(clicked).toHaveLength(0);
	});

	it('leaving the page aborts the preflight and never starts the download', async () => {
		let seen: AbortSignal | undefined;
		let finish: (() => void) | undefined;
		h.preflight.mockImplementation(
			(_slug: string, _opts: unknown, signal?: AbortSignal) =>
				new Promise<void>((resolve) => {
					seen = signal;
					finish = resolve;
				})
		);
		const btn = await renderOwner('ws-leave');
		await fireEvent.click(btn);
		await screen.findByRole('button', { name: /Preparing export/ });
		cleanup();
		expect(seen?.aborted).toBe(true);
		// Even a preflight that settles after the page is gone clicks nothing.
		finish?.();
		await tick();
		await new Promise((r) => setTimeout(r, 0));
		expect(clicked).toHaveLength(0);
	});
});
