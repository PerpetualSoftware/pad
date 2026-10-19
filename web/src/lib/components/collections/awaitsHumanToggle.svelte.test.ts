import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Collection } from '$lib/types';

/**
 * TASK-3919 (IDEA-3906): the collection editor's "Items are asks of a person"
 * toggle reads settings.awaits_human and writes it back on save, false
 * included, since the server carries a stored key across a write that omits it.
 */
const updateMock = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...a: unknown[]) => updateMock(...a),
			list: vi.fn().mockResolvedValue([]),
			delete: vi.fn(),
			fieldUsage: vi.fn().mockResolvedValue({ fields: {} })
		},
		items: { listByCollection: vi.fn().mockResolvedValue([]) }
	},
	isConflictOrNotFound: () => false
}));

const { default: EditCollectionModal } = await import('./EditCollectionModal.svelte');

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
	updateMock.mockReset();
	updateMock.mockImplementation(async () => ({}));
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

function collectionWith(settings: Record<string, unknown>): Collection {
	return {
		id: 'c1',
		slug: 'human-tasks',
		name: 'Human tasks',
		icon: '',
		description: '',
		prefix: 'HT',
		schema: JSON.stringify({
			fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], terminal_options: ['done'] }]
		}),
		settings: JSON.stringify(settings)
	} as unknown as Collection;
}

function toggle(): HTMLInputElement {
	const label = [...document.querySelectorAll<HTMLLabelElement>('label.setting-toggle')].find((l) =>
		/asks of a person/i.test(l.textContent ?? '')
	);
	expect(label, 'the awaits_human toggle').toBeTruthy();
	return label!.querySelector('input[type="checkbox"]')!;
}

async function saveAndReadSettings(): Promise<Record<string, unknown>> {
	const save = [...document.querySelectorAll<HTMLButtonElement>('button')].find((b) =>
		/^\s*save/i.test(b.textContent ?? '')
	);
	expect(save, 'a Save button').toBeTruthy();
	save!.click();
	await settle();
	expect(updateMock).toHaveBeenCalledTimes(1);
	const data = updateMock.mock.calls[0][2] as { settings: string };
	return JSON.parse(data.settings);
}

describe('EditCollectionModal awaits_human toggle (TASK-3919)', () => {
	it('starts off for a collection without the key, and saves true once checked', async () => {
		app = mount(EditCollectionModal, {
			target: host,
			props: { open: true, collection: collectionWith({}), wsSlug: 'ws', initialSection: 'display' }
		}) as Record<string, unknown>;
		await settle();

		const box = toggle();
		expect(box.checked).toBe(false);
		box.click();
		await settle();

		expect((await saveAndReadSettings()).awaits_human).toBe(true);
	});

	it('starts on for a stored true, and saves an explicit false once unchecked', async () => {
		app = mount(EditCollectionModal, {
			target: host,
			props: { open: true, collection: collectionWith({ awaits_human: true }), wsSlug: 'ws', initialSection: 'display' }
		}) as Record<string, unknown>;
		await settle();

		const box = toggle();
		expect(box.checked).toBe(true);
		box.click();
		await settle();

		const settings = await saveAndReadSettings();
		expect('awaits_human' in settings).toBe(true);
		expect(settings.awaits_human).toBe(false);
	});
});
