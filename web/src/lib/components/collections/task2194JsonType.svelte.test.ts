import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Collection } from '$lib/types';
import { fieldFromDef, FIELD_TYPES, type EditableField } from './field-editor-types';

/**
 * TASK-2194 (audit C123): a json field in the schema editor. FIELD_TYPES
 * omitted json, so such a field (Pad's own Playbooks.arguments) rendered a
 * blank type select, and any change converted it to a scalar type. It is
 * locked to json now, never offered for a new field, and saves as json.
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


describe('a json field is locked to its type', () => {
	it('shows json in a disabled select, with no other choice', async () => {
		const field: EditableField = $state(fieldFromDef({ key: 'arguments', label: 'Arguments', type: 'json' }));
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();
		const select = host.querySelector<HTMLSelectElement>('select[aria-label="Field type"]')!;
		expect(select.disabled).toBe(true);
		expect(select.value).toBe('json');
		expect([...select.options].map((o) => o.value)).toEqual(['json']);
		expect(host.textContent).toContain('type locked');
	});

	it('CONTROL: a text field has the full, enabled dropdown, and json is not offered', async () => {
		const field: EditableField = $state(fieldFromDef({ key: 'notes', label: 'Notes', type: 'text' }));
		app = mount(FieldEditor, { target: host, props: { field, index: 0, total: 1 } }) as Record<string, unknown>;
		await settle();
		const select = host.querySelector<HTMLSelectElement>('select[aria-label="Field type"]')!;
		expect(select.disabled).toBe(false);
		expect(select.value).toBe('text');
		expect([...select.options].map((o) => o.value)).toEqual(FIELD_TYPES);
		expect(FIELD_TYPES).not.toContain('json');
	});

	it('a collection holding a json field saves it as json', async () => {
		updateMock.mockImplementation(async () => ({}));
		const collection = {
			id: 'c1', slug: 'playbooks', name: 'Playbooks', icon: '', description: '', prefix: 'PLAYB',
			schema: JSON.stringify({
				fields: [
					{ key: 'status', label: 'Status', type: 'select', options: ['draft', 'active'] },
					{ key: 'arguments', label: 'Arguments', type: 'json' }
				]
			}),
			settings: '{}',
			updated_at: '2026-10-09T00:00:00Z'
		} as unknown as Collection;
		app = mount(EditCollectionModal, {
			target: host,
			props: { open: true, collection, wsSlug: 'ws', initialSection: 'fields' }
		}) as Record<string, unknown>;
		await settle();
		const save = [...document.querySelectorAll<HTMLButtonElement>('button')].find((b) => /^\s*save/i.test(b.textContent ?? ''));
		expect(save, 'a Save button').toBeTruthy();
		save!.click();
		await settle();
		expect(updateMock).toHaveBeenCalledTimes(1);
		const data = updateMock.mock.calls[0][2] as { schema: string };
		const args = JSON.parse(data.schema).fields.find((f: { key: string }) => f.key === 'arguments');
		expect(args.type).toBe('json');
	});
});
