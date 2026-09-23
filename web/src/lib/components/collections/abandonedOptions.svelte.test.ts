import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Collection } from '$lib/types';
import { fieldFromDef, keptAbandoned, type EditableField } from './field-editor-types';

/**
 * BUG-2347 PR 2: the schema editor's "counts as abandoned" toggle, and the
 * round trip that makes it safe. Before this, EditableField had no abandoned
 * list, so SAVING a collection in the web editor rebuilt every FieldDef
 * without `abandoned_options` and silently stripped the declaration.
 */
const updateMock = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...a: unknown[]) => updateMock(...a),
			list: vi.fn().mockResolvedValue([]),
			delete: vi.fn()
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

describe('keptAbandoned', () => {
	it('keeps only abandoned values still being saved as terminal', () => {
		expect(keptAbandoned(['cancelled', 'dropped'], ['done', 'cancelled'])).toEqual(['cancelled']);
	});
});

describe('FieldEditor abandoned toggle', () => {
	function statusField(): EditableField {
		return fieldFromDef(
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['open', 'done', 'cancelled'],
				terminal_options: ['done', 'cancelled']
			},
			true
		);
	}

	function row(option: string): HTMLElement {
		const input = [...host.querySelectorAll<HTMLInputElement>('.option-name-input')].find((i) => i.value === option);
		expect(input, `a row for ${option}`).toBeTruthy();
		return input!.closest('.option-row') as HTMLElement;
	}

	it('offers the toggle only on terminal options, and toggles the field state', async () => {
		const field = $state(statusField());
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();

		expect(row('open').querySelector('.option-abandoned-toggle')).toBeNull();
		const toggle = row('cancelled').querySelector<HTMLButtonElement>('.option-abandoned-toggle')!;
		expect(toggle).toBeTruthy();

		toggle.click();
		await settle();
		expect(field.abandonedOptions).toEqual(['cancelled']);
		expect(toggle.getAttribute('aria-pressed')).toBe('true');

		// Unmarking the option as terminal clears its abandoned mark too.
		row('cancelled').querySelector<HTMLButtonElement>('.option-done-toggle')!.click();
		await settle();
		expect(field.terminalOptions).toEqual(['done']);
		expect(field.abandonedOptions).toEqual([]);
	});
});

describe('FieldEditor option rename (codex round 1 on #1468)', () => {
	function typeInto(input: HTMLInputElement, value: string) {
		input.value = value;
		input.dispatchEvent(new Event('input', { bubbles: true }));
	}

	it('carries terminal and abandoned marks to the renamed value, keystroke by keystroke', async () => {
		const field = $state(
			fieldFromDef(
				{
					key: 'status',
					label: 'Status',
					type: 'select',
					options: ['open', 'done', 'cancelled'],
					terminal_options: ['done', 'cancelled'],
					abandoned_options: ['cancelled']
				},
				true
			)
		);
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();

		const input = [...host.querySelectorAll<HTMLInputElement>('.option-name-input')].find((i) => i.value === 'cancelled')!;
		for (const step of ['cancelle', 'cancele', 'canceled']) {
			typeInto(input, step);
			await settle();
		}
		expect(field.options).toEqual(['open', 'done', 'canceled']);
		expect(field.terminalOptions).toEqual(['done', 'canceled']);
		expect(field.abandonedOptions).toEqual(['canceled']);
	});

	it('leaves a mark on the old value while another row still holds it', async () => {
		const field = $state(
			fieldFromDef(
				{
					key: 'status',
					label: 'Status',
					type: 'select',
					options: ['done', 'done'],
					terminal_options: ['done']
				},
				true
			)
		);
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();
		const inputs = host.querySelectorAll<HTMLInputElement>('.option-name-input');
		typeInto(inputs[1], 'shipped');
		await settle();
		expect(field.terminalOptions).toEqual(['done']);
	});
});

describe('EditCollectionModal round trip', () => {
	it('saves abandoned_options it loaded, instead of stripping them', async () => {
		updateMock.mockImplementation(async () => ({}));
		const collection = {
			id: 'c1',
			slug: 'plans',
			name: 'Plans',
			icon: '',
			description: '',
			prefix: 'PLAN',
			schema: JSON.stringify({
				fields: [
					{
						key: 'status',
						label: 'Status',
						type: 'select',
						options: ['open', 'completed', 'overturned'],
						terminal_options: ['completed', 'overturned'],
						abandoned_options: ['overturned']
					}
				]
			}),
			settings: '{}',
			updated_at: '2026-09-23T00:00:00Z'
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
		const data = updateMock.mock.calls[0][2] as { schema: string };
		const status = JSON.parse(data.schema).fields.find((f: { key: string }) => f.key === 'status');
		expect(status.abandoned_options).toEqual(['overturned']);
	});

	it('saves the RENAMED value as terminal and abandoned, with the value migration', async () => {
		updateMock.mockImplementation(async () => ({}));
		const collection = {
			id: 'c2',
			slug: 'plans2',
			name: 'Plans',
			icon: '',
			description: '',
			prefix: 'PLANB',
			schema: JSON.stringify({
				fields: [
					{
						key: 'status',
						label: 'Status',
						type: 'select',
						options: ['open', 'completed', 'overturned'],
						terminal_options: ['completed', 'overturned'],
						abandoned_options: ['overturned']
					}
				]
			}),
			settings: '{}',
			updated_at: '2026-09-23T00:00:00Z'
		} as unknown as Collection;

		app = mount(EditCollectionModal, {
			target: host,
			props: { open: true, collection, wsSlug: 'ws', initialSection: 'fields' }
		}) as Record<string, unknown>;
		await settle();

		const input = [...document.querySelectorAll<HTMLInputElement>('.option-name-input')].find(
			(i) => i.value === 'overturned'
		);
		expect(input, 'the overturned option row').toBeTruthy();
		input!.value = 'reversed';
		input!.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();

		[...document.querySelectorAll<HTMLButtonElement>('button')].find((b) => /^\s*save/i.test(b.textContent ?? ''))!.click();
		await settle();

		const data = updateMock.mock.calls[0][2] as { schema: string; migrations?: { rename_options?: Record<string, string> }[] };
		const status = JSON.parse(data.schema).fields.find((f: { key: string }) => f.key === 'status');
		expect(status.terminal_options).toEqual(['completed', 'reversed']);
		expect(status.abandoned_options).toEqual(['reversed']);
		expect(JSON.stringify(data.migrations ?? [])).toContain('reversed');
	});
});
