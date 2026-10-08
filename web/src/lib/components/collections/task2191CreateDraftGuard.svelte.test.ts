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
		const ask = vi.spyOn(window, 'confirm');
		cancel();
		expect(ask).not.toHaveBeenCalled();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('Cancel on an edited draft asks, and a No keeps it open', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		const ask = vi.spyOn(window, 'confirm').mockReturnValue(false);
		cancel();
		expect(ask).toHaveBeenCalledTimes(1);
		expect(onclose).not.toHaveBeenCalled();
		ask.mockReturnValue(true);
		cancel();
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('Escape goes through the same rule', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		vi.spyOn(window, 'confirm').mockReturnValue(false);
		escape();
		expect(onclose).not.toHaveBeenCalled();
	});

	it('Back to templates asks too, and a No keeps the draft', async () => {
		await openOnBlank();
		typeName('Deals');
		await settle();
		vi.spyOn(window, 'confirm').mockReturnValue(false);
		document.querySelector<HTMLButtonElement>('button[aria-label="Back to templates"]')!.click();
		await settle();
		expect(document.querySelector<HTMLInputElement>('input[placeholder="Collection name"]')?.value).toBe('Deals');
	});
});
