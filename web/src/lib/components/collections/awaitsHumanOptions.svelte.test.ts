import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Collection } from '$lib/types';
import { fieldFromDef, keptAwaitsHuman, type EditableField } from './field-editor-types';

/**
 * TASK-4643: the schema editor's "waits on a person" toggle
 * (`awaits_human_options`), and the round trip that keeps a save from
 * stripping the declaration. A terminal option cannot also wait on a person.
 */
const updateMock = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...a: unknown[]) => updateMock(...a),
			list: vi.fn().mockResolvedValue([]),
			delete: vi.fn(),
			// No item holds any value, so no save here is confirmed first (TASK-2188).
			fieldUsage: vi.fn().mockResolvedValue({ fields: {} })
		},
		items: { listByCollection: vi.fn().mockResolvedValue([]) }
	},
	isConflictOrNotFound: () => false
}));

const { default: FieldEditor } = await import('./FieldEditor.svelte');
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
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('keptAwaitsHuman', () => {
	it('keeps only waiting values still saved as options and not terminal', () => {
		expect(keptAwaitsHuman(['review', 'gone', 'done'], ['open', 'review', 'done'], ['done'])).toEqual(['review']);
	});
});

describe('fieldFromDef waiting values', () => {
	it('respells a declaration the server matched trimmed and case-insensitively, so a save keeps it', () => {
		const f = fieldFromDef(
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['open', 'review', 'done'],
				terminal_options: ['done'],
				awaits_human_options: [' Review ', 'REVIEW', 'gone']
			},
			true
		);
		expect(f.awaitsHumanOptions).toEqual(['review', 'gone']);
		expect(keptAwaitsHuman(f.awaitsHumanOptions, f.options, f.terminalOptions)).toEqual(['review']);
	});
});

function row(option: string): HTMLElement {
	const input = [...document.querySelectorAll<HTMLInputElement>('.option-name-input')].find((i) => i.value === option);
	expect(input, `a row for ${option}`).toBeTruthy();
	return input!.closest('.option-row') as HTMLElement;
}

function statusField(extra: Record<string, unknown> = {}): EditableField {
	return fieldFromDef(
		{
			key: 'status',
			label: 'Status',
			type: 'select',
			options: ['open', 'review', 'done'],
			terminal_options: ['done'],
			...extra
		},
		true
	);
}

describe('FieldEditor waiting toggle', () => {
	it('offers the toggle only on non-terminal options, and toggles the field state', async () => {
		const field = $state(statusField());
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();

		expect(row('done').querySelector('.option-waits-toggle')).toBeNull();
		const toggle = row('review').querySelector<HTMLButtonElement>('.option-waits-toggle')!;
		expect(toggle).toBeTruthy();
		expect(toggle.getAttribute('aria-pressed')).toBe('false');

		toggle.click();
		await settle();
		expect(field.awaitsHumanOptions).toEqual(['review']);
		expect(toggle.getAttribute('aria-pressed')).toBe('true');
	});

	it('clears the waiting mark when the option is marked terminal', async () => {
		const field = $state(statusField({ awaits_human_options: ['review'] }));
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();

		row('review').querySelector<HTMLButtonElement>('.option-done-toggle')!.click();
		await settle();
		expect(field.terminalOptions).toEqual(['done', 'review']);
		expect(field.awaitsHumanOptions).toEqual([]);
		expect(row('review').querySelector('.option-waits-toggle')).toBeNull();
	});

	it('carries the waiting mark to a renamed option', async () => {
		const field = $state(statusField({ awaits_human_options: ['review'] }));
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();

		const input = row('review').querySelector<HTMLInputElement>('.option-name-input')!;
		input.value = 'in-review';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		expect(field.awaitsHumanOptions).toEqual(['in-review']);
	});
});

describe('EditCollectionModal round trip', () => {
	async function saveSchema(fields: unknown[]): Promise<{ key: string; awaits_human_options?: string[] }[]> {
		updateMock.mockImplementation(async () => ({}));
		const collection = {
			id: 'c1',
			slug: 'reviews',
			name: 'Reviews',
			icon: '',
			description: '',
			prefix: 'REV',
			schema: JSON.stringify({ fields }),
			settings: '{}',
			updated_at: '2026-11-18T00:00:00Z'
		} as unknown as Collection;
		app = mount(EditCollectionModal, {
			target: host,
			props: { open: true, collection, wsSlug: 'ws', initialSection: 'fields' }
		}) as Record<string, unknown>;
		await settle();
		const save = [...document.querySelectorAll<HTMLButtonElement>('button')].find((b) =>
			/^\s*save/i.test(b.textContent ?? '')
		);
		expect(save, 'a Save button').toBeTruthy();
		save!.click();
		await settle();
		expect(updateMock).toHaveBeenCalledTimes(1);
		return JSON.parse((updateMock.mock.calls[0][2] as { schema: string }).schema).fields;
	}

	it('saves awaits_human_options it loaded, instead of stripping them', async () => {
		const fields = await saveSchema([
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'review', 'done'], terminal_options: ['done'], awaits_human_options: ['review'] }
		]);
		expect(fields.find((f) => f.key === 'status')!.awaits_human_options).toEqual(['review']);
	});

	it('saves them on a field that declares no terminal options', async () => {
		const fields = await saveSchema([
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], terminal_options: ['done'] },
			{ key: 'review', label: 'Review', type: 'select', options: ['asked', 'given'], awaits_human_options: ['asked'] }
		]);
		expect(fields.find((f) => f.key === 'review')!.awaits_human_options).toEqual(['asked']);
	});
});
