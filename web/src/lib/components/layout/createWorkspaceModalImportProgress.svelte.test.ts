import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import { ImportTransportError } from '$lib/api/importUpload';

/**
 * BUG-3475 — the import dialog sat on "Importing..." forever when the upload
 * stalled. It now shows the upload's progress, then a finishing phase, and
 * when no response arrives it asks the server what became of the attempt and
 * says exactly that: finished, kept, nothing kept, or unknown.
 */

type ImportOpts = {
	importKey?: string;
	onProgress?: (sent: number, total: number) => void;
	onUploaded?: () => void;
	signal?: AbortSignal;
};

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	templates: { list: vi.fn() },
	workspaces: {
		create: vi.fn(),
		importBundle: vi.fn(),
		importStatus: vi.fn(),
		list: vi.fn(),
		get: vi.fn(),
		me: vi.fn()
	}
}));
const goto = vi.hoisted(() => vi.fn());
vi.mock('$lib/api/client', () => ({ api, isPlanLimitError: () => false, planLimitMessage: () => '' }));
vi.mock('$app/navigation', () => ({ goto }));
const toastShow = vi.hoisted(() => vi.fn());
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: toastShow, dismiss: vi.fn(), get toasts() { return []; } }
}));

import CreateWorkspaceModal from './CreateWorkspaceModal.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { uiStore } from '$lib/stores/ui.svelte';

const WS = { id: 'w1', slug: 'imported', name: 'Imported', owner_username: 'alice' };

async function settle(): Promise<void> {
	for (let i = 0; i < 24; i++) {
		await Promise.resolve();
		await tick();
	}
}

function btn(container: HTMLElement, label: RegExp): HTMLButtonElement {
	const found = [...container.querySelectorAll('button')].find((b) => label.test(b.textContent?.trim() ?? ''));
	if (!found) throw new Error(`no button matching ${label}: ${[...container.querySelectorAll('button')].map((b) => b.textContent?.trim()).join(' | ')}`);
	return found as HTMLButtonElement;
}

async function attachBundle(container: HTMLElement): Promise<void> {
	btn(container, /^Import$/).click();
	await settle();
	const input = container.querySelector('input[type="file"]');
	if (!input) throw new Error('import panel did not render');
	const file = new File([new Uint8Array(2048)], 'bundle.tar.gz', { type: 'application/gzip' });
	Object.defineProperty(input, 'files', { value: [file], configurable: true });
	await fireEvent.input(input);
	await settle();
}

/** importBundle that hands its options to the test and waits to be told how to end. */
function controlledImport() {
	let opts!: ImportOpts;
	let resolve!: (v: unknown) => void;
	let reject!: (e: unknown) => void;
	api.workspaces.importBundle.mockImplementation((_f: File, _n: string, o: ImportOpts) => {
		opts = o;
		return new Promise((res, rej) => {
			resolve = res;
			reject = rej;
			o.signal?.addEventListener('abort', () => rej(new ImportTransportError('aborted')));
		});
	});
	return {
		get opts() {
			return opts;
		},
		resolve: (v: unknown) => resolve(v),
		reject: (e: unknown) => reject(e)
	};
}

beforeEach(async () => {
	goto.mockClear();
	toastShow.mockClear();
	api.templates.list.mockResolvedValue([]);
	api.workspaces.list.mockResolvedValue([]);
	api.workspaces.get.mockResolvedValue(WS);
	api.workspaces.me.mockResolvedValue(null);
	api.workspaces.importBundle.mockReset();
	api.workspaces.importStatus.mockReset();
	authStore.clear();
	api.auth.session.mockResolvedValue({ authenticated: true, user: { id: 'u1', email: 'u1@example.com' } });
	await authStore.load();
	uiStore.openCreateWorkspace();
});

afterEach(() => {
	vi.useRealTimers();
	cleanup();
	uiStore.closeCreateWorkspace();
	authStore.clear();
});

describe('BUG-3475: the import dialog shows where an import is', () => {
	it('shows upload progress, then the finishing phase, and sends a key per attempt', async () => {
		const imp = controlledImport();
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();

		expect(imp.opts.importKey).toMatch(/^[0-9a-f]{32}$/);
		imp.opts.onProgress?.(1024, 2048);
		await settle();
		expect(container.textContent).toContain('Uploading 1.0 KB of 2.0 KB');
		expect(container.querySelector('progress')?.getAttribute('value')).toBe('1024');

		imp.opts.onUploaded?.();
		await settle();
		expect(container.textContent).toContain('Upload complete. Finishing the import');
		expect(container.textContent).not.toContain('Importing...');

		imp.resolve(WS);
		await settle();
		expect(goto).toHaveBeenCalledWith('/alice/imported');
	});

	it('a stall the server rolled back says nothing was kept and offers to try again', async () => {
		const imp = controlledImport();
		api.workspaces.importStatus.mockResolvedValue({ state: 'removed' });
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();

		imp.reject(new ImportTransportError('stalled'));
		await settle();

		expect(api.workspaces.importStatus).toHaveBeenCalledWith(imp.opts.importKey);
		const alert = container.querySelector('[role="alert"]');
		expect(alert?.textContent).toContain('The upload stopped making progress');
		expect(alert?.textContent).toContain('Nothing was kept');
		expect(btn(container, /^Try again$/).disabled).toBe(false);
		expect(goto).not.toHaveBeenCalled();
	});

	it('a lost 201 is reported as the finished import it was, with a way in', async () => {
		const imp = controlledImport();
		api.workspaces.importStatus.mockResolvedValue({
			state: 'complete',
			workspace_slug: 'imported',
			workspace_name: 'Imported',
			owner_username: 'alice'
		});
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();
		imp.opts.onUploaded?.();
		imp.reject(new ImportTransportError('no_response'));
		await settle();

		expect(container.querySelector('[role="alert"]')?.textContent).toContain('The import finished: "Imported" is ready');
		expect(container.textContent).not.toContain('Try again');
		btn(container, /^Open workspace$/).click();
		await settle();
		expect(goto).toHaveBeenCalledWith('/alice/imported');
	});

	it('a key the server does not know is UNKNOWN, never "nothing was kept"', async () => {
		vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
		const imp = controlledImport();
		api.workspaces.importStatus.mockResolvedValue(null);
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();
		imp.reject(new ImportTransportError('network'));
		await settle();
		expect(container.textContent).toContain('Checking with the server what was imported');

		await vi.advanceTimersByTimeAsync(80_000);
		await settle();
		const alert = container.querySelector('[role="alert"]');
		expect(alert?.textContent).toContain('check your workspace list');
		expect(alert?.textContent).not.toContain('Nothing was kept');
		// It kept asking for the whole window before saying so.
		expect(api.workspaces.importStatus.mock.calls.length).toBeGreaterThan(20);
	});

	it('"Stop upload" aborts the upload, and the outcome is still resolved', async () => {
		const imp = controlledImport();
		api.workspaces.importStatus.mockResolvedValue({ state: 'removed' });
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();

		btn(container, /^Stop upload$/).click();
		await settle();
		expect(imp.opts.signal?.aborted).toBe(true);
		expect(container.querySelector('[role="alert"]')?.textContent).toContain('The import was cancelled. Nothing was kept');
	});

	it('closing the dialog mid-upload aborts it and does not go on asking for the outcome', async () => {
		const imp = controlledImport();
		api.workspaces.importStatus.mockResolvedValue({ state: 'removed' });
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();

		btn(container, /^Cancel$/).click();
		await settle();
		expect(imp.opts.signal?.aborted).toBe(true);
		expect(api.workspaces.importStatus).not.toHaveBeenCalled();
	});

	it('closing while the outcome is being resolved leaves nothing on the next open', async () => {
		const imp = controlledImport();
		let answer!: (v: unknown) => void;
		api.workspaces.importStatus.mockReturnValue(new Promise((r) => (answer = r)));
		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /^Import Workspace$/).click();
		await settle();
		imp.reject(new ImportTransportError('stalled'));
		await settle();
		expect(container.textContent).toContain('Checking with the server');

		uiStore.closeCreateWorkspace();
		await settle();
		uiStore.openCreateWorkspace();
		await settle();
		// The reopened dialog starts on Create; the outcome panel lives on the
		// Import tab, so look there, where a stale write would show.
		btn(container, /^Import$/).click();
		await settle();
		answer({ state: 'removed' });
		await settle();
		expect(container.querySelector('[role="alert"]')).toBeNull();
		expect(container.textContent).not.toContain('Nothing was kept');
	});
});
