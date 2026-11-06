import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { Item } from '$lib/types';

const mocks = vi.hoisted(() => ({ preflight: vi.fn(), copy: vi.fn() }));
vi.mock('$lib/api/client', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/api/client')>();
	return {
		...actual,
		api: {
			workspaces: {
				list: async () => [
					{ id: 'a', slug: 'source', name: 'Source' },
					{ id: 'b', slug: 'dest', name: 'Destination' }
				],
				me: async () => ({ role: 'owner' })
			},
			collections: {
				list: async () => [{ id: 'tasks', slug: 'tasks', name: 'Tasks', schema: '{"fields":[]}' }]
			},
			items: { copyPreflight: mocks.preflight, copy: mocks.copy }
		}
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
		user: { id: 'owner' },
		userId: 'owner',
		authenticated: true
	}
}));

import { PadApiError } from '$lib/api/client';
import CopyItemDialog from './CopyItemDialog.svelte';

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

it('shows the attachment cap refusal before submission and disables Copy', async () => {
	const message = 'This item references more than 1,000 attachments, which is too many to copy. Remove some references, then copy it again.';
	mocks.preflight.mockRejectedValue(new PadApiError({ code: 'too_many_attachments', message }));
	vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (this: HTMLDialogElement) {
		this.open = true;
	});
	vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (this: HTMLDialogElement) {
		this.open = false;
	});
	render(CopyItemDialog, {
		open: true,
		onclose: vi.fn(),
		sourceWsSlug: 'source',
		sourceCollectionSlug: 'notes',
		sourceRef: 'NOTE-1',
		item: { id: 'item', slug: 'item', title: 'Source item', fields: '{}' } as Item,
		flushContent: async () => true,
		onmove: async () => ({ status: 'ok' as const }),
		oncopied: vi.fn()
	});
	await screen.findByRole('option', { name: 'Destination' });
	await fireEvent.change(screen.getByLabelText('Workspace'), { target: { value: 'dest' } });
	await screen.findByRole('option', { name: 'Tasks' });
	await fireEvent.change(screen.getByLabelText('Collection'), { target: { value: 'tasks' } });
	await waitFor(() => expect(screen.getByRole('alert').textContent).toBe(message));
	const button = screen.getByRole('button', { name: 'Copy' }) as HTMLButtonElement;
	expect(button.disabled).toBe(true);
	await fireEvent.click(button);
	expect(mocks.copy).not.toHaveBeenCalled();
});
