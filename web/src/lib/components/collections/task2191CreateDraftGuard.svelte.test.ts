import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-2191: the Create Collection dialog dropped a draft (name, fields,
 * options, settings) on Escape, a backdrop click, the ✕, Cancel or Back to
 * templates, without asking. Every way out now goes through one rule: an
 * edited draft asks first, an untouched one closes as before.
 */
vi.mock('$lib/api/client', () => ({
	api: { collections: { list: vi.fn().mockResolvedValue([]), create: vi.fn() } }
}));

const { default: CreateCollectionModal } = await import('./CreateCollectionModal.svelte');
// The question is asked through the shared dialog (TASK-3543); the test
// answers it through the store.
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

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	onclose.mockReset();
});
afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	confirmDialog.abandonAll();
	vi.restoreAllMocks();
});

async function openOnBlank() {
	app = mount(CreateCollectionModal, {
		target: host,
		props: { open: true, wsSlug: 'ws', oncreated: vi.fn(), onclose }
	}) as Record<string, unknown>;
	await settle();
	document.querySelector<HTMLButtonElement>('.template-card--blank')!.click();
	await settle();
}

function typeName(value: string) {
	const input = document.querySelector<HTMLInputElement>('input[placeholder="Collection name"]')!;
	input.value = value;
	input.dispatchEvent(new Event('input', { bubbles: true }));
}

const escape = () =>
	document.querySelector('dialog')!.dispatchEvent(new Event('cancel', { cancelable: true }));
const cancel = () =>
	[...document.querySelectorAll<HTMLButtonElement>('button.btn-cancel')].find((b) => /cancel/i.test(b.textContent ?? ''))!.click();

describe('TASK-2191: the Create Collection dialog keeps a draft', () => {
	it('an untouched draft closes without asking', async () => {
		await openOnBlank();
		cancel();
		await settle();
		expect(confirmDialog.active).toBeNull();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('Cancel on an edited draft asks, and a No keeps it open', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		cancel();
		await settle();
		expect(confirmDialog.active?.confirmLabel).toBe('Discard');
		confirmDialog.cancel();
		await settle();
		expect(onclose).not.toHaveBeenCalled();
		cancel();
		await settle();
		confirmDialog.confirm();
		await settle();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('a second Cancel while the question is open asks nothing more (TASK-3543)', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		cancel();
		await settle();
		cancel();
		escape();
		await settle();
		confirmDialog.confirm();
		await settle();
		expect(confirmDialog.active).toBeNull();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('Escape goes through the same rule', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		escape();
		await settle();
		expect(confirmDialog.active).not.toBeNull();
		confirmDialog.cancel();
		await settle();
		expect(onclose).not.toHaveBeenCalled();
	});

	it('Back to templates asks too, and a No keeps the draft', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		document.querySelector<HTMLButtonElement>('button[aria-label="Back to templates"]')!.click();
		await settle();
		expect(confirmDialog.active).not.toBeNull();
		confirmDialog.cancel();
		await settle();
		expect(document.querySelector<HTMLInputElement>('input[placeholder="Collection name"]')?.value).toBe('Deals');
	});
});
