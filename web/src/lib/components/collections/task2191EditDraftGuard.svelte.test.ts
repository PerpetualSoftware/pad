import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Collection } from '$lib/types';

/**
 * TASK-2191: the Edit Collection dialog dropped every edit on Escape, a
 * backdrop click, the ✕ or Cancel, without asking. They now go through one
 * rule: an edited form asks first, an untouched one closes as before, and a
 * successful save re-baselines so closing after it does not ask.
 */
const updateMock = vi.hoisted(() => vi.fn());
vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...a: unknown[]) => updateMock(...a),
			fieldUsage: vi.fn().mockResolvedValue({ fields: {} }),
			list: vi.fn().mockResolvedValue([]),
			delete: vi.fn()
		},
		items: { listByCollection: vi.fn().mockResolvedValue([]) }
	},
	isConflictOrNotFound: () => false
}));

const { default: EditCollectionModal } = await import('./EditCollectionModal.svelte');
const { confirmDialog } = await import('$lib/stores/confirmDialog.svelte');

let host: HTMLElement;
let app: Record<string, unknown> | null = null;
const onclose = vi.fn();

async function settle(): Promise<void> {
	flushSync();
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

const collection = {
	id: 'c1',
	slug: 'deals',
	name: 'Deals',
	icon: '',
	description: '',
	prefix: 'DEAL',
	schema: JSON.stringify({ fields: [{ key: 'stage', label: 'Stage', type: 'select', options: ['todo', 'done'] }] }),
	settings: '{}',
	updated_at: '2026-10-08T00:00:00Z'
} as unknown as Collection;

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	onclose.mockReset();
	updateMock.mockReset();
	updateMock.mockImplementation(async () => ({ ...collection, updated_at: '2026-10-08T00:00:01Z' }));
});
afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	confirmDialog.abandonAll();
	vi.restoreAllMocks();
});

let props: { open: boolean } & Record<string, unknown>;

async function open() {
	const p = $state({ open: true, collection, wsSlug: 'ws', onupdated: vi.fn(), onclose });
	props = p;
	app = mount(EditCollectionModal, { target: host, props: p as never }) as Record<string, unknown>;
	await settle();
}

function rename(value: string) {
	const input = [...document.querySelectorAll<HTMLInputElement>('input')].find((i) => i.value === 'Deals')!;
	input.value = value;
	input.dispatchEvent(new Event('input', { bubbles: true }));
}

const cancel = () =>
	[...document.querySelectorAll<HTMLButtonElement>('button.btn-cancel')].find((b) => /^\s*cancel\s*$/i.test(b.textContent ?? ''))!.click();
const escape = () => document.querySelector('dialog')!.dispatchEvent(new Event('cancel', { cancelable: true }));

describe('TASK-2191: the Edit Collection dialog keeps edits', () => {
	it('an untouched form closes without asking', async () => {
		await open();
		cancel();
		expect(confirmDialog.active).toBeNull();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('Cancel and Escape on an edited form ask, and a No keeps it open', async () => {
		await open();
		rename('Pipeline');
		await settle();
		cancel();
		await settle();
		expect(confirmDialog.active?.title).toBe('Discard your changes?');
		confirmDialog.cancel();
		await settle();
		escape();
		await settle();
		expect(confirmDialog.active).not.toBeNull();
		confirmDialog.cancel();
		await settle();
		expect(onclose).not.toHaveBeenCalled();
		cancel();
		await settle();
		confirmDialog.confirm();
		await settle();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('a yes given after the dialog closed does not close it again (TASK-3543)', async () => {
		await open();
		rename('Pipeline');
		await settle();
		cancel();
		await settle();
		props.open = false; // the host closed it while the question was open
		await settle();
		confirmDialog.confirm();
		await settle();
		expect(onclose).not.toHaveBeenCalled();
	});

	it('closing while a save is in flight does not ask: the save owns the edits', async () => {
		let finish: (v: unknown) => void = () => {};
		updateMock.mockImplementation(() => new Promise((r) => (finish = r)));
		await open();
		rename('Pipeline');
		await settle();
		[...document.querySelectorAll<HTMLButtonElement>('button')].find((b) => /^\s*save changes/i.test(b.textContent ?? ''))!.click();
		await settle();
		cancel();
		expect(confirmDialog.active).toBeNull();
		expect(onclose).toHaveBeenCalledTimes(1);
		finish({ ...collection });
		await settle();
	});

	it('after a successful save, closing does not ask', async () => {
		await open();
		rename('Pipeline');
		await settle();
		[...document.querySelectorAll<HTMLButtonElement>('button')].find((b) => /^\s*save changes/i.test(b.textContent ?? ''))!.click();
		await settle();
		expect(updateMock).toHaveBeenCalledTimes(1);
		cancel();
		expect(confirmDialog.active).toBeNull();
		expect(onclose).toHaveBeenCalledTimes(1);
	});
});
