/**
 * BUG-3095 row 9 — the DRIVEN leg for `ItemAttachmentStrip`'s delete
 * confirmation.
 *
 * WHY THIS ONE NEEDED ITS OWN LEG, and why the population guard could never
 * have found it. Every other member of this surface suspends on an `await`.
 * This one suspends on a HUMAN: `requestDelete` opens a non-blocking in-app
 * confirmation menu and returns, and `confirmDelete` sends the DELETE when the
 * user presses the destructive row. There is no `await` anywhere between those
 * two points, so no post-await analysis — the guard's whole model — applies.
 *
 * The site already understood the shape and got the quantity wrong. Its comment
 * reasons it out correctly ("`window.confirm` blocked the thread … so the fence
 * — and the permission — are re-checked HERE, at the point that actually sends
 * the request"), but the re-check was `paint.isCurrent()`, which compares
 * `{ws, item}`. A logout and a different login while the menu is open moves
 * NEITHER, so the navigation fence passes and the DELETE goes out on the new
 * user's cookie — a destructive call, for a deletion the previous user asked
 * for.
 *
 * The auth double is local to this file rather than the family's shared one,
 * because the sibling suite `ItemAttachmentStrip.svelte.test.ts` deliberately
 * does NOT mock `authStore`: it uses the real store, whose epoch never moves
 * under test, so `identityFence()` is inert there and every existing assertion
 * is unaffected. Mocking it here keeps that true.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { AttachmentListResponse } from '$lib/types';

const listMock =
	vi.fn<(ws: string, filters: Record<string, unknown>) => Promise<AttachmentListResponse>>();
const deleteMock = vi.fn<(ws: string, id: string) => Promise<void>>();

// Declared via `vi.hoisted` rather than as a top-level class: `vi.mock`'s
// factory is hoisted above every module-scope binding, so a plain `class`
// referenced from inside it throws `Cannot access 'FakeApiError' before
// initialization` at mock time.
const { FakeApiError } = vi.hoisted(() => ({
	FakeApiError: class extends Error {
		code: string;
		constructor(code: string) {
			super(code);
			this.code = code;
		}
	},
}));

vi.mock('$lib/api/client', () => ({
	PadApiError: FakeApiError,
	api: {
		attachments: {
			list: (ws: string, filters: Record<string, unknown>) => listMock(ws, filters),
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
			delete: (ws: string, id: string) => deleteMock(ws, id),
		},
	},
}));

const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		get identityEpoch() {
			return epoch;
		},
		get userId() {
			return 'u1';
		},
		get user() {
			return { id: 'u1', name: 'A' };
		},
		get authenticated() {
			return true;
		},
		/** What the real `notifyIdentityChange` does to the epoch. */
		moveIdentity() {
			epoch += 1;
		},
		reset() {
			epoch = 0;
		},
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		/**
		 * Required because `toast.svelte.ts` subscribes at MODULE LOAD, and the
		 * strip imports it. A double missing this throws before any test runs —
		 * so the double has to model the store's subscription surface, not just
		 * the one method under test.
		 */
		onIdentityChange(_fn: (previousUserId: string) => void) {
			return () => {};
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import ItemAttachmentStrip from './ItemAttachmentStrip.svelte';

const ATT = {
	id: 'att-1',
	filename: 'notes.pdf',
	mime_type: 'application/pdf',
	size_bytes: 1024,
	created_at: '2026-07-20T00:00:00Z',
	item_id: 'item-1',
};

let target: HTMLElement;
let instance: ReturnType<typeof mount> | null = null;

function deleteButtons(): HTMLButtonElement[] {
	return Array.from(target.querySelectorAll<HTMLButtonElement>('.att-delete'));
}
function confirmRow(label: string): HTMLElement | undefined {
	return Array.from(document.querySelectorAll<HTMLElement>('[role="menu"] [role="menuitem"]')).find(
		(el) => el.querySelector('.mi-label')?.textContent?.trim() === label
	);
}

async function settle() {
	await Promise.resolve();
	await Promise.resolve();
	flushSync();
}

beforeEach(() => {
	listMock.mockReset();
	deleteMock.mockReset();
	deleteMock.mockResolvedValue(undefined);
	auth.reset();
	listMock.mockResolvedValue({
		attachments: [ATT],
		total: 1,
	} as unknown as AttachmentListResponse);
	target = document.body.appendChild(document.createElement('div'));
});

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	target.remove();
	document.querySelectorAll('[role="menu"]').forEach((n) => n.remove());
});

async function mountAndOpenConfirm() {
	instance = mount(ItemAttachmentStrip, {
		target,
		props: {
			wsSlug: 'ws',
			username: 'dave',
			itemId: 'item-1',
			canDelete: true,
			itemContent: null,
			liveContent: null,
			hostToken: 'host-1',
			parentArchived: false,
			mutationsEnabled: true,
		} as Record<string, unknown>,
	});
	flushSync();
	await settle();
	const btns = deleteButtons();
	// PRECONDITION for every absence assertion below: the delete control is
	// actually reachable. Without this, a strip that rendered no tile at all
	// would make "no DELETE was sent" pass against the defect.
	expect(btns.length).toBeGreaterThan(0);
	btns[0].click();
	flushSync();
	expect(confirmRow('Delete file')).toBeTruthy();
}

describe('ItemAttachmentStrip delete confirmation identity fence (BUG-3095 row 9)', () => {
	it('CONTROL: confirming with the identity unchanged DOES send the DELETE', async () => {
		await mountAndOpenConfirm();
		confirmRow('Delete file')!.click();
		flushSync();
		await settle();
		expect(deleteMock).toHaveBeenCalledWith('ws', 'att-1');
	});

	it('an identity change while the confirmation is OPEN refuses the DELETE', async () => {
		await mountAndOpenConfirm();

		// The window: the menu is up, the user is replaced, and then the
		// destructive row is pressed.
		auth.moveIdentity();
		flushSync();

		confirmRow('Delete file')!.click();
		flushSync();
		await settle();

		expect(deleteMock).not.toHaveBeenCalled();
	});

	it('the refusal is the IDENTITY fence, not the navigation fence', async () => {
		// Neither the workspace nor the item moves in this test, so
		// `paint.isCurrent()` is true throughout. If the DELETE is still refused,
		// the only check that could have refused it is the identity one. Without
		// this leg, a green above would be consistent with the pre-existing
		// navigation fence having done the work — which would leave the real bug
		// open, since that fence provably cannot see a user swap.
		await mountAndOpenConfirm();

		const epochBefore = auth.identityEpoch;
		auth.moveIdentity();
		flushSync();
		expect(auth.identityEpoch).not.toBe(epochBefore);

		confirmRow('Delete file')!.click();
		flushSync();
		await settle();

		expect(deleteMock).not.toHaveBeenCalled();
	});

	it('the fence is captured per confirmation, not once per component', async () => {
		// A second `×` click replaces the pending record and must capture afresh.
		// If the predicate were a component-level field captured at mount, an
		// identity change would poison every later confirmation for the life of
		// the strip — including legitimate ones by the new user.
		await mountAndOpenConfirm();
		auth.moveIdentity();
		flushSync();
		confirmRow('Delete file')!.click();
		flushSync();
		await settle();
		expect(deleteMock).not.toHaveBeenCalled();

		// Now the NEW identity opens its own confirmation and presses it.
		const btns = deleteButtons();
		expect(btns.length).toBeGreaterThan(0);
		btns[0].click();
		flushSync();
		confirmRow('Delete file')!.click();
		flushSync();
		await settle();
		expect(deleteMock).toHaveBeenCalledWith('ws', 'att-1');
	});
});
