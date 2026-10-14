// BUG-3792 (Codex r1 Medium): a password-protected share's retry goes through
// api.share.get, the loader that walks every page of a shared collection. It
// used to fetch the first page by hand, so a protected collection past one page
// rendered incomplete.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';

const share = vi.hoisted(() => ({ calls: [] as unknown[][] }));

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {}
	return {
		PadApiError,
		withRequestDeadline: vi.fn(),
		api: {
			share: {
				get: vi.fn(async (...args: unknown[]) => {
					share.calls.push(args);
					if (args[1] === undefined) return { require_password: true };
					return { type: 'collection', collection: { name: 'Protected' }, items: [{ ref: 'TASK-1', title: 'One' }] };
				})
			}
		}
	};
});

import { page } from '$app/state';
import SharePage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

afterEach(() => {
	cleanup();
	share.calls = [];
});

describe('share page: the password retry walks the share through api.share.get (BUG-3792)', () => {
	it('submits the password to the paged loader', async () => {
		page.params = { token: 'tok' };
		const { container } = render(SharePage);
		await settle();
		const input = container.querySelector('form.password-form input') as HTMLInputElement;
		expect(input, 'the password form did not render').not.toBeNull();
		await fireEvent.input(input, { target: { value: 'pw' } });
		await fireEvent.submit(container.querySelector('form.password-form')!);
		await settle();
		expect(share.calls).toEqual([['tok'], ['tok', 'pw']]);
		expect(container.querySelector('form.password-form')).toBeNull();
	});
});
