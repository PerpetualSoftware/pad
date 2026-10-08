import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Collection, CollectionFieldUsage } from '$lib/types';
import { schemaEditImpacts, schemaEditNeedsTyping } from './schemaEditImpact';

/**
 * TASK-2187: removing an option was read as a POSITIONAL rename, so removing
 * "b" of [a, b, c] sent b→c and the server moved every "b" item to "c".
 * TASK-2188: removing a field or an option said nothing about the items
 * holding it. Now a removal sends no migration, the editor lists what the
 * edit does to items, and the save asks first.
 */
const updateMock = vi.hoisted(() => vi.fn());
const usageMock = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...a: unknown[]) => updateMock(...a),
			fieldUsage: (...a: unknown[]) => usageMock(...a),
			list: vi.fn().mockResolvedValue([]),
			delete: vi.fn()
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
	usageMock.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

function collection(): Collection {
	return {
		id: 'c1',
		slug: 'deals',
		name: 'Deals',
		icon: '',
		description: '',
		prefix: 'DEAL',
		schema: JSON.stringify({
			fields: [
				{ key: 'stage', label: 'Stage', type: 'select', options: ['todo', 'doing', 'done'], terminal_options: ['done'] },
				{ key: 'note', label: 'Note', type: 'text' }
			]
		}),
		settings: '{}',
		updated_at: '2026-10-08T00:00:00Z'
	} as unknown as Collection;
}

const usage = (stage: Record<string, number>, note = 0): CollectionFieldUsage => ({
	fields: {
		stage: { items: Object.values(stage).reduce((a, b) => a + b, 0), values: stage },
		note: { items: note }
	}
});

async function open(): Promise<void> {
	app = mount(EditCollectionModal, {
		target: host,
		props: { open: true, collection: collection(), wsSlug: 'ws', initialSection: 'fields' }
	}) as Record<string, unknown>;
	await settle();
}

function removeOption(option: string) {
	const input = [...document.querySelectorAll<HTMLInputElement>('.option-name-input')].find((i) => i.value === option);
	expect(input, `a row for ${option}`).toBeTruthy();
	input!.closest('.option-row')!.querySelector<HTMLButtonElement>('.option-remove-btn')!.click();
}

const button = (re: RegExp) => [...document.querySelectorAll<HTMLButtonElement>('button')].find((b) => re.test(b.textContent ?? ''));
const save = () => button(/^\s*save changes/i)!;

type Sent = { schema: string; migrations?: unknown[] };
const sent = (): Sent => updateMock.mock.calls[0][2] as Sent;
const stageOptions = (d: Sent) => JSON.parse(d.schema).fields.find((f: { key: string }) => f.key === 'stage').options;

describe('removing an option is never a rename (TASK-2187)', () => {
	for (const option of ['todo', 'doing', 'done']) {
		it(`removing "${option}" sends no migration`, async () => {
			usageMock.mockResolvedValue(usage({}));
			await open();
			removeOption(option);
			await settle();
			save().click();
			await settle();
			expect(updateMock).toHaveBeenCalledTimes(1);
			expect(sent().migrations ?? []).toEqual([]);
			expect(stageOptions(sent())).toEqual(['todo', 'doing', 'done'].filter((o) => o !== option));
		});
	}

	it('a rename after a removal still migrates the renamed option only', async () => {
		usageMock.mockResolvedValue(usage({}));
		await open();
		removeOption('todo');
		await settle();
		const input = [...document.querySelectorAll<HTMLInputElement>('.option-name-input')].find((i) => i.value === 'doing')!;
		input.value = 'in progress';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		save().click();
		await settle();
		expect(sent().migrations).toEqual([{ field: 'stage', rename_options: { doing: 'in progress' } }]);
	});
});

describe('the edit says what it does to items (TASK-2188)', () => {
	it('names the items an option removal leaves behind, and confirms before saving', async () => {
		usageMock.mockResolvedValue(usage({ todo: 1, doing: 3 }));
		await open();
		removeOption('doing');
		await settle();
		expect(document.querySelector('.impact-notices')?.textContent).toContain('3 items have “doing” in “Stage”');

		save().click();
		await settle();
		expect(updateMock).not.toHaveBeenCalled();
		const confirm = document.querySelector('.impact-confirm');
		expect(confirm?.textContent).toContain('3 items have “doing”');
		// Under the threshold: no name to type.
		expect(confirm?.querySelector('input')).toBeNull();

		button(/save anyway/i)!.click();
		await settle();
		expect(updateMock).toHaveBeenCalledTimes(1);
	});

	it('a removal no item holds saves without asking', async () => {
		usageMock.mockResolvedValue(usage({ todo: 2 }));
		await open();
		removeOption('done');
		await settle();
		expect(document.querySelector('.impact-notices')).toBeNull();
		save().click();
		await settle();
		expect(updateMock).toHaveBeenCalledTimes(1);
	});

	it('a large total asks for the collection name', async () => {
		usageMock.mockResolvedValue(usage({ doing: 30 }));
		await open();
		removeOption('doing');
		await settle();
		save().click();
		await settle();
		const anyway = button(/save anyway/i)!;
		expect(anyway.disabled).toBe(true);
		const typed = document.querySelector<HTMLInputElement>('.impact-confirm input')!;
		typed.value = 'Deals';
		typed.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		expect(anyway.disabled).toBe(false);
		anyway.click();
		await settle();
		expect(updateMock).toHaveBeenCalledTimes(1);
	});

	it('an UNKNOWN count is never read as none: it asks, with the name typed', async () => {
		usageMock.mockRejectedValue(new Error('offline'));
		await open();
		removeOption('done');
		await settle();
		expect(document.querySelector('.impact-notices')?.textContent).toContain('An unknown number of items');
		save().click();
		await settle();
		expect(updateMock).not.toHaveBeenCalled();
		expect(document.querySelector('.impact-confirm input')).not.toBeNull();
	});

	it('Undo puts a removed option back where it was, and the save changes nothing', async () => {
		usageMock.mockResolvedValue(usage({ doing: 3 }));
		await open();
		removeOption('doing');
		await settle();
		button(/^undo$/i)!.click();
		await settle();
		expect(document.querySelector('.impact-notices')).toBeNull();
		save().click();
		await settle();
		expect(stageOptions(sent())).toEqual(['todo', 'doing', 'done']);
		expect(sent().migrations ?? []).toEqual([]);
	});

	it('names a removed field and Undo restores it', async () => {
		usageMock.mockResolvedValue(usage({}, 4));
		await open();
		const noteCard = [...document.querySelectorAll<HTMLElement>('.field-card')].find((c) =>
			[...c.querySelectorAll<HTMLInputElement>('input')].some((i) => i.value === 'Note')
		);
		expect(noteCard, 'the Note field').toBeTruthy();
		const remove = noteCard!.querySelector<HTMLButtonElement>('.field-remove-btn');
		expect(remove, 'the remove-field control').toBeTruthy();
		remove!.click();
		await settle();
		expect(document.querySelector('.impact-notices')?.textContent).toContain('4 items have a value in “Note”');
		button(/^undo$/i)!.click();
		await settle();
		expect(document.querySelector('.impact-notices')).toBeNull();
	});
});

describe('schemaEditImpacts', () => {
	const seeded = [{ key: 'stage', label: 'Stage', type: 'select', options: ['a', 'b'] }];
	it('a type change away from select orphans nothing', () => {
		expect(schemaEditImpacts(seeded, [{ key: 'stage', type: 'text', options: [] }], [], usage({ a: 1 }))).toEqual([]);
	});
	it('a renamed option is a move, not a removal', () => {
		expect(
			schemaEditImpacts(seeded, [{ key: 'stage', type: 'select', options: ['x', 'b'] }], [{ field: 'stage', rename_options: { a: 'x' } }], usage({ a: 2 }))
		).toEqual([{ kind: 'rename', field: 'stage', label: 'Stage', from: 'a', to: 'x', items: 2 }]);
	});
	it('typing is asked at 25 items or any unknown count', () => {
		expect(schemaEditNeedsTyping([{ kind: 'field', field: 'f', label: 'F', items: 24 }])).toBe(false);
		expect(schemaEditNeedsTyping([{ kind: 'field', field: 'f', label: 'F', items: 25 }])).toBe(true);
		expect(schemaEditNeedsTyping([{ kind: 'field', field: 'f', label: 'F', items: undefined }])).toBe(true);
	});
});
