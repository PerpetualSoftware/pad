import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

const apiMock = vi.hoisted(() => ({
	listCollectionGrants: vi.fn(),
	listItemGrants: vi.fn(),
	deleteItemGrant: vi.fn(),
	deleteCollectionGrant: vi.fn(),
	listItemShareLinks: vi.fn(),
	listCollectionShareLinks: vi.fn(),
	createItemShareLink: vi.fn(),
	deleteShareLink: vi.fn(),
}));

vi.mock('$lib/api/client', () => ({
	api: {
		grants: {
			listCollectionGrants: (...a: unknown[]) => apiMock.listCollectionGrants(...a),
			listItemGrants: (...a: unknown[]) => apiMock.listItemGrants(...a),
			deleteCollectionGrant: (...a: unknown[]) => apiMock.deleteCollectionGrant(...a),
			deleteItemGrant: (...a: unknown[]) => apiMock.deleteItemGrant(...a),
			createCollectionGrant: vi.fn(),
			createItemGrant: vi.fn(),
		},
		shareLinks: {
			listCollectionShareLinks: (...a: unknown[]) => apiMock.listCollectionShareLinks(...a),
			listItemShareLinks: (...a: unknown[]) => apiMock.listItemShareLinks(...a),
			createCollectionShareLink: vi.fn(),
			createItemShareLink: (...a: unknown[]) => apiMock.createItemShareLink(...a),
			deleteShareLink: (...a: unknown[]) => apiMock.deleteShareLink(...a),
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
		// `toast.svelte.ts` subscribes at MODULE LOAD and the dialog imports it, so
		// a double without this throws before any test runs.
		onIdentityChange(_fn: (p: string) => void) {
			return () => {};
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import ShareDialog from './ShareDialog.svelte';

let target: HTMLElement;
let instance: ReturnType<typeof mount> | null = null;

function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (reason?: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

async function settle() {
	await Promise.resolve();
	await Promise.resolve();
	await new Promise((r) => setTimeout(r, 0));
	flushSync();
}

function render() {
	target = document.body.appendChild(document.createElement('div'));
	instance = mount(ShareDialog, {
		target,
		props: {
			wsSlug: 'ws',
			type: 'item',
			targetSlug: 'ITEM-1',
			targetName: 'Item One',
			open: true,
		} as Record<string, unknown>,
	});
	flushSync();
}

beforeEach(() => {
	auth.reset();
	for (const fn of Object.values(apiMock)) (fn as ReturnType<typeof vi.fn>).mockReset();
	apiMock.listItemGrants.mockResolvedValue([]);
	apiMock.listItemShareLinks.mockResolvedValue([]);
	apiMock.deleteItemGrant.mockResolvedValue(undefined);
});

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	if (target) target.remove();
	vi.clearAllMocks();
});

describe('ShareDialog: share link created while initial list loads', () => {
	it('keeps the token-bearing created link when the pending list resolves empty', async () => {
		const listD = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listD.promise);
		apiMock.createItemShareLink.mockResolvedValue({
			id: 'n1',
			token: 'tok-1',
			view_count: 0,
			created_at: '2026-10-02T00:00:00Z',
			require_auth: false,
		});

		render();
		await settle();

		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();
		expect(apiMock.createItemShareLink).toHaveBeenCalledTimes(1);

		listD.resolve([]);
		await settle();

		const highlight = target.querySelector('.new-link-highlight');
		expect(highlight).toBeTruthy();
		expect(highlight?.textContent).toContain('copy it now');
		const urlInput = target.querySelector<HTMLInputElement>('input.new-link-url');
		expect(urlInput).toBeTruthy();
		expect(urlInput?.value).toContain('tok-1');
	});

	it('preserves the token-bearing link and avoids duplicates when the list includes the created id without a token', async () => {
		const listD = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listD.promise);
		apiMock.createItemShareLink.mockResolvedValue({
			id: 'n1',
			token: 'tok-1',
			view_count: 0,
			created_at: '2026-10-02T00:00:00Z',
			require_auth: false,
		});

		render();
		await settle();

		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();
		expect(apiMock.createItemShareLink).toHaveBeenCalledTimes(1);

		listD.resolve([
			{ id: 'n1', view_count: 0, created_at: '2026-10-02T00:00:00Z', require_auth: false },
			{ id: 'old1', view_count: 3, created_at: '2026-10-01T00:00:00Z', require_auth: false },
		]);
		await settle();

		expect(target.querySelectorAll('.share-link-row')).toHaveLength(2);
		expect(target.querySelectorAll('.new-link-highlight')).toHaveLength(1);
		expect(target.querySelector('.new-link-highlight')?.textContent).toContain('copy it now');
		const urlInputs = target.querySelectorAll<HTMLInputElement>('input.new-link-url');
		expect(urlInputs).toHaveLength(1);
		expect(urlInputs[0].value).toContain('tok-1');
		expect(target.querySelectorAll('.link-url-hidden')).toHaveLength(1);
		expect(target.textContent).toContain('3 views');
	});

	it('renders the load error without throwing when a link is created before the list rejects', async () => {
		const listD = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listD.promise);
		apiMock.createItemShareLink.mockResolvedValue({
			id: 'n1',
			token: 'tok-1',
			view_count: 0,
			created_at: '2026-10-02T00:00:00Z',
			require_auth: false,
		});

		render();
		await settle();

		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();
		expect(apiMock.createItemShareLink).toHaveBeenCalledTimes(1);

		listD.reject(new Error('failed to load links'));
		await expect(settle()).resolves.toBeUndefined();

		const errorEl = target.querySelector('.share-links-section .error-msg');
		expect(errorEl).toBeTruthy();
		expect(errorEl?.textContent).toContain('failed to load links');
	});
});

describe('ShareDialog: the list load does not hide a link created during it (BUG-4892)', () => {
	const createdLink = {
		id: 'n1',
		token: 'tok-1',
		view_count: 0,
		created_at: '2026-10-02T00:00:00Z',
		require_auth: false,
	};

	it('a list that fails after a create still shows the one-time URL beside the error', async () => {
		const listD = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listD.promise);
		apiMock.createItemShareLink.mockResolvedValue(createdLink);

		render();
		await settle();

		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();

		listD.reject(new Error('failed to load links'));
		await settle();

		const section = target.querySelector('.share-links-section')!;
		expect(section.querySelector('.error-msg')?.textContent).toContain('failed to load links');
		const urlInput = section.querySelector<HTMLInputElement>('input.new-link-url');
		expect(urlInput).toBeTruthy();
		expect(urlInput?.value).toContain('tok-1');
		expect(section.textContent).not.toContain('No share links yet');
	});

	it('a link created while the list is pending shows at once, with no Loading text', async () => {
		const listD = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listD.promise);
		apiMock.createItemShareLink.mockResolvedValue(createdLink);

		render();
		await settle();

		const section = target.querySelector('.share-links-section')!;
		expect(section.textContent).toContain('Loading...');

		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();

		expect(section.querySelector<HTMLInputElement>('input.new-link-url')?.value).toContain('tok-1');
		expect(section.textContent).not.toContain('Loading...');
		expect(section.textContent).not.toContain('No share links yet');

		listD.resolve([]);
		await settle();
	});

	it('the empty state shows only after a successful empty load', async () => {
		const listD = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listD.promise);

		render();
		await settle();

		const section = target.querySelector('.share-links-section')!;
		expect(section.textContent).not.toContain('No share links yet');

		listD.reject(new Error('failed to load links'));
		await settle();

		expect(section.querySelector('.error-msg')?.textContent).toContain('failed to load links');
		expect(section.textContent).not.toContain('No share links yet');
		expect(section.querySelector('.share-links-list')).toBeNull();
	});
});

describe('ShareDialog: a created link does not outlive its open', () => {
	const props = $state({ wsSlug: 'ws', type: 'item', targetSlug: 'ITEM-1', targetName: 'Item One', open: true });

	function renderReactive() {
		props.targetSlug = 'ITEM-1';
		props.open = true;
		target = document.body.appendChild(document.createElement('div'));
		instance = mount(ShareDialog, { target, props: props as Record<string, unknown> });
		flushSync();
	}

	async function createOneLink() {
		apiMock.createItemShareLink.mockResolvedValue({
			id: 'n1',
			token: 'tok-1',
			view_count: 0,
			created_at: '2026-10-02T00:00:00Z',
			require_auth: false,
		});
		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();
		expect(target.querySelector<HTMLInputElement>('input.new-link-url')?.value).toContain('tok-1');
	}

	async function reopen() {
		props.open = false;
		flushSync();
		await settle();
		props.open = true;
		flushSync();
		await settle();
	}

	it('reopening the same target shows the created link without its token', async () => {
		renderReactive();
		await settle();
		await createOneLink();

		apiMock.listItemShareLinks.mockResolvedValue([
			{ id: 'n1', view_count: 0, created_at: '2026-10-02T00:00:00Z', require_auth: false },
		]);
		await reopen();

		expect(target.querySelectorAll('.share-link-row')).toHaveLength(1);
		expect(target.querySelector('.new-link-highlight')).toBeNull();
		expect(target.querySelectorAll('.link-url-hidden')).toHaveLength(1);
		expect(target.innerHTML).not.toContain('tok-1');
	});

	it('reopening for another target does not show the previous target link', async () => {
		renderReactive();
		await settle();
		await createOneLink();

		apiMock.listItemShareLinks.mockResolvedValue([]);
		props.open = false;
		flushSync();
		await settle();
		props.targetSlug = 'ITEM-2';
		props.open = true;
		flushSync();
		await settle();

		expect(apiMock.listItemShareLinks).toHaveBeenLastCalledWith('ws', 'ITEM-2');
		expect(target.querySelectorAll('.share-link-row')).toHaveLength(0);
		expect(target.innerHTML).not.toContain('tok-1');
	});

	it('a create still in flight when the dialog reopens for another target does not land there', async () => {
		renderReactive();
		await settle();

		const createD = deferred<unknown>();
		apiMock.createItemShareLink.mockReturnValue(createD.promise);
		target.querySelector<HTMLButtonElement>('.create-link-btn')!.click();
		await settle();

		const listB = deferred<unknown[]>();
		apiMock.listItemShareLinks.mockReturnValue(listB.promise);
		props.open = false;
		flushSync();
		await settle();
		props.targetSlug = 'ITEM-2';
		props.open = true;
		flushSync();
		await settle();

		createD.resolve({
			id: 'n1',
			token: 'tok-1',
			view_count: 0,
			created_at: '2026-10-02T00:00:00Z',
			require_auth: false,
		});
		await settle();
		listB.resolve([]);
		await settle();

		expect(target.querySelectorAll('.share-link-row')).toHaveLength(0);
		expect(target.querySelector('.new-link-highlight')).toBeNull();
		expect(target.innerHTML).not.toContain('tok-1');
	});
});
