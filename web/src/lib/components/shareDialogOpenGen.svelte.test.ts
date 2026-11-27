/**
 * BUG-4893: the dialog stays mounted between opens and can reopen for another
 * target, so a response that belongs to an earlier open must not commit into
 * the current one. Each leg starts a request in open #1, closes, reopens, then
 * settles the open-#1 promise and asserts nothing from it lands.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

const apiMock = vi.hoisted(() => ({
	listCollectionGrants: vi.fn(),
	listItemGrants: vi.fn(),
	createItemGrant: vi.fn(),
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
			createItemGrant: (...a: unknown[]) => apiMock.createItemGrant(...a),
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

const auth = vi.hoisted(() => ({
	get identityEpoch() {
		return 0;
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
	identityFence() {
		return () => true;
	},
	onIdentityChange(_fn: (p: string) => void) {
		return () => {};
	},
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

const toastShow = vi.hoisted(() => vi.fn());
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (...a: unknown[]) => toastShow(...a), dismiss: () => {} },
}));

import ShareDialog from './ShareDialog.svelte';

let target: HTMLElement;
let instance: ReturnType<typeof mount> | null = null;

const props = $state({ wsSlug: 'ws', type: 'item', targetSlug: 'ITEM-1', targetName: 'Item One', open: true });

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

async function render() {
	props.wsSlug = 'ws';
	props.targetSlug = 'ITEM-1';
	props.open = true;
	target = document.body.appendChild(document.createElement('div'));
	instance = mount(ShareDialog, { target, props: props as Record<string, unknown> });
	flushSync();
	await settle();
}

async function reopen(nextSlug = 'ITEM-2') {
	props.open = false;
	flushSync();
	await settle();
	props.targetSlug = nextSlug;
	props.open = true;
	flushSync();
	await settle();
}

function grantsSection() {
	return target.querySelector('.grants-section')!;
}

function addSection() {
	return target.querySelector('.add-section')!;
}

function shareBtn() {
	return target.querySelector<HTMLButtonElement>('.share-btn')!;
}

function createBtn() {
	return target.querySelector<HTMLButtonElement>('.create-link-btn')!;
}

function typeEmail(value: string) {
	const input = target.querySelector<HTMLInputElement>('.email-input')!;
	input.value = value;
	input.dispatchEvent(new Event('input', { bubbles: true }));
	flushSync();
}

async function startShare(value: string) {
	typeEmail(value);
	shareBtn().click();
	await settle();
}

async function startRevoke() {
	grantsSection().querySelector<HTMLButtonElement>('.revoke-btn')!.click();
	flushSync();
	grantsSection().querySelector<HTMLButtonElement>('.revoke-confirm-yes')!.click();
	await settle();
}

const grantA = { id: 'g1', user_id: 'u9', user_email: 'old@example.com', permission: 'view' };
const grantB = { id: 'g2', user_id: 'u8', user_email: 'new@example.com', permission: 'view' };

beforeEach(() => {
	for (const fn of Object.values(apiMock)) (fn as ReturnType<typeof vi.fn>).mockReset();
	toastShow.mockReset();
	apiMock.listItemGrants.mockResolvedValue([]);
	apiMock.listItemShareLinks.mockResolvedValue([]);
});

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	if (target) target.remove();
	vi.clearAllMocks();
});

describe('ShareDialog: an earlier open does not commit into a reopened dialog (BUG-4893)', () => {
	it('loadGrants success from open #1 does not replace the open #2 list', async () => {
		const d1 = deferred<unknown[]>();
		apiMock.listItemGrants.mockReturnValueOnce(d1.promise);
		await render();

		apiMock.listItemGrants.mockResolvedValue([grantB]);
		await reopen();
		expect(grantsSection().textContent).toContain('new@example.com');

		d1.resolve([grantA]);
		await settle();

		expect(grantsSection().textContent).toContain('new@example.com');
		expect(grantsSection().textContent).not.toContain('old@example.com');
	});

	it('loadGrants failure from open #1 does not paint its error into open #2', async () => {
		const d1 = deferred<unknown[]>();
		apiMock.listItemGrants.mockReturnValueOnce(d1.promise);
		await render();

		apiMock.listItemGrants.mockResolvedValue([grantB]);
		await reopen();

		d1.reject(new Error('grants exploded'));
		await settle();

		expect(grantsSection().textContent).not.toContain('grants exploded');
		expect(grantsSection().textContent).toContain('new@example.com');
	});

	it('loadGrants from open #1 settling does not end the open #2 loading state', async () => {
		const d1 = deferred<unknown[]>();
		const d2 = deferred<unknown[]>();
		apiMock.listItemGrants.mockReturnValueOnce(d1.promise).mockReturnValueOnce(d2.promise);
		await render();
		await reopen();
		expect(grantsSection().textContent).toContain('Loading...');

		d1.resolve([grantA]);
		await settle();

		expect(grantsSection().textContent).toContain('Loading...');
		d2.resolve([]);
		await settle();
		expect(grantsSection().textContent).toContain('No one has been granted access yet.');
	});

	it('handleShare success from open #1 on another target does not append or reset the open #2 form, and still toasts', async () => {
		const shareD = deferred<unknown>();
		apiMock.createItemGrant.mockReturnValue(shareD.promise);
		await render();
		await startShare('old@example.com');
		expect(apiMock.createItemGrant).toHaveBeenCalledTimes(1);

		await reopen();
		typeEmail('typing@example.com');

		shareD.resolve(grantA);
		await settle();

		expect(grantsSection().textContent).not.toContain('old@example.com');
		expect(target.querySelector<HTMLInputElement>('.email-input')!.value).toBe('typing@example.com');
		expect(toastShow).toHaveBeenCalledWith('Shared with old@example.com', 'success');
	});

	it('handleShare success from open #1 on the SAME target lists the grant in open #2, once', async () => {
		const shareD = deferred<unknown>();
		apiMock.createItemGrant.mockReturnValue(shareD.promise);
		await render();
		await startShare('old@example.com');

		await reopen('ITEM-1');
		shareD.resolve(grantA);
		await settle();
		expect(grantsSection().querySelectorAll('.grant-row').length).toBe(1);
		expect(grantsSection().textContent).toContain('old@example.com');
	});

	it('handleShare success from open #1 is not listed twice when the open #2 load already has it', async () => {
		const shareD = deferred<unknown>();
		apiMock.createItemGrant.mockReturnValue(shareD.promise);
		await render();
		await startShare('old@example.com');

		apiMock.listItemGrants.mockResolvedValue([grantA]);
		await reopen('ITEM-1');
		shareD.resolve(grantA);
		await settle();
		expect(grantsSection().querySelectorAll('.grant-row').length).toBe(1);
	});

	it('handleShare failure from open #1 is a toast in open #2, not an inline error', async () => {
		const shareD = deferred<unknown>();
		apiMock.createItemGrant.mockReturnValue(shareD.promise);
		await render();
		await startShare('old@example.com');

		await reopen();

		shareD.reject(new Error('share exploded'));
		await settle();

		expect(addSection().textContent).not.toContain('share exploded');
		expect(addSection().querySelector('.error-msg')).toBeNull();
		expect(toastShow).toHaveBeenCalledWith('share exploded', 'error');
	});

	it('a share pending from open #1 leaves the open #2 Share button idle, and its end does not end an open #2 share', async () => {
		const share1 = deferred<unknown>();
		const share2 = deferred<unknown>();
		apiMock.createItemGrant.mockReturnValueOnce(share1.promise).mockReturnValueOnce(share2.promise);
		await render();
		await startShare('old@example.com');
		expect(shareBtn().textContent).toContain('Sharing...');

		await reopen();
		typeEmail('new@example.com');
		expect(shareBtn().textContent?.trim()).toBe('Share');
		expect(shareBtn().disabled).toBe(false);

		await startShare('new@example.com');
		expect(shareBtn().textContent).toContain('Sharing...');

		share1.resolve(grantA);
		await settle();
		expect(shareBtn().textContent).toContain('Sharing...');

		share2.resolve(grantB);
		await settle();
	});

	it('handleRevoke success from open #1 removes the revoked grant from a same-target open #2, and toasts', async () => {
		apiMock.listItemGrants.mockResolvedValue([grantA]);
		const revokeD = deferred<unknown>();
		apiMock.deleteItemGrant.mockReturnValue(revokeD.promise);
		await render();
		await startRevoke();
		expect(apiMock.deleteItemGrant).toHaveBeenCalledTimes(1);

		await reopen('ITEM-1');
		expect(grantsSection().textContent).toContain('old@example.com');

		revokeD.resolve(undefined);
		await settle();

		expect(grantsSection().textContent).not.toContain('old@example.com');
		expect(toastShow).toHaveBeenCalledWith('Access revoked', 'success');
	});

	it('handleRevoke failure from open #1 is still reported in open #2', async () => {
		apiMock.listItemGrants.mockResolvedValue([grantA]);
		const revokeD = deferred<unknown>();
		apiMock.deleteItemGrant.mockReturnValue(revokeD.promise);
		await render();
		await startRevoke();

		await reopen('ITEM-1');

		revokeD.reject(new Error('revoke exploded'));
		await settle();

		expect(toastShow).toHaveBeenCalledWith('revoke exploded', 'error');
		expect(grantsSection().textContent).toContain('old@example.com');
	});

	it('a revoke pending from open #1 does not block, or end, a revoke in open #2', async () => {
		apiMock.listItemGrants.mockResolvedValue([grantA]);
		const revoke1 = deferred<unknown>();
		const revoke2 = deferred<unknown>();
		apiMock.deleteItemGrant.mockReturnValueOnce(revoke1.promise).mockReturnValueOnce(revoke2.promise);
		await render();
		await startRevoke();

		// Open #2 lists grantB first, so its revoke targets a different grant
		// from the one open #1 is still revoking.
		apiMock.listItemGrants.mockResolvedValue([grantB, grantA]);
		await reopen('ITEM-1');
		const btn = () => grantsSection().querySelector<HTMLButtonElement>('.revoke-btn')!;
		expect(btn().disabled).toBe(false);

		await startRevoke();
		expect(apiMock.deleteItemGrant).toHaveBeenCalledTimes(2);
		expect(btn().disabled).toBe(true);

		revoke1.resolve(undefined);
		await settle();
		expect(btn().disabled).toBe(true);

		revoke2.resolve(undefined);
		await settle();
	});

	it('handleCreateShareLink failure from open #1 is still reported in open #2', async () => {
		const createD = deferred<unknown>();
		apiMock.createItemShareLink.mockReturnValue(createD.promise);
		await render();
		createBtn().click();
		await settle();

		await reopen();

		createD.reject(new Error('create exploded'));
		await settle();

		expect(toastShow).toHaveBeenCalledWith('create exploded', 'error');
	});

	it('a create pending from open #1 leaves the open #2 Create button idle and enabled', async () => {
		const createD = deferred<unknown>();
		apiMock.createItemShareLink.mockReturnValue(createD.promise);
		await render();
		createBtn().click();
		await settle();
		expect(createBtn().textContent).toContain('Creating...');

		await reopen();

		expect(createBtn().textContent?.trim()).toBe('+ Create link');
		expect(createBtn().disabled).toBe(false);

		createD.reject(new Error('late'));
		await settle();
	});

	it('a create from open #1 ending does not end a create in open #2', async () => {
		const create1 = deferred<unknown>();
		const create2 = deferred<unknown>();
		apiMock.createItemShareLink.mockReturnValueOnce(create1.promise).mockReturnValueOnce(create2.promise);
		await render();
		createBtn().click();
		await settle();

		await reopen();
		createBtn().click();
		await settle();
		expect(apiMock.createItemShareLink).toHaveBeenCalledTimes(2);
		expect(createBtn().textContent).toContain('Creating...');

		create1.reject(new Error('late'));
		await settle();
		expect(createBtn().textContent).toContain('Creating...');

		create2.reject(new Error('late'));
		await settle();
	});

	it('a share-link delete from open #1 ending does not end a delete in open #2', async () => {
		const link = { id: 'l1', view_count: 0, created_at: '2026-10-02T00:00:00Z', require_auth: false };
		const link2 = { id: 'l2', view_count: 0, created_at: '2026-10-02T00:00:00Z', require_auth: false };
		apiMock.listItemShareLinks.mockResolvedValueOnce([link]).mockResolvedValueOnce([link2]);
		const del1 = deferred<unknown>();
		const del2 = deferred<unknown>();
		apiMock.deleteShareLink.mockReturnValueOnce(del1.promise).mockReturnValueOnce(del2.promise);
		const linksSection = () => target.querySelector('.share-links-section')!;
		const startDelete = async () => {
			linksSection().querySelector<HTMLButtonElement>('.revoke-btn')!.click();
			flushSync();
			linksSection().querySelector<HTMLButtonElement>('.revoke-confirm-yes')!.click();
			await settle();
		};

		await render();
		await startDelete();

		await reopen();
		const btn = () => linksSection().querySelector<HTMLButtonElement>('.revoke-btn')!;
		expect(btn().disabled).toBe(false);

		await startDelete();
		expect(apiMock.deleteShareLink).toHaveBeenCalledTimes(2);
		expect(btn().disabled).toBe(true);

		del1.reject(new Error('late'));
		await settle();
		expect(btn().disabled).toBe(true);

		del2.resolve(undefined);
		await settle();
	});

	it('a create from open #1 that succeeds after a same-target reopen shows its one-time URL there', async () => {
		const createD = deferred<unknown>();
		apiMock.createItemShareLink.mockReturnValue(createD.promise);
		await render();
		createBtn().click();
		await settle();

		await reopen('ITEM-1');
		createD.resolve({ id: 'l1', token: 'tok-same', view_count: 0, created_at: '2026-11-27T00:00:00Z' });
		await settle();

		const url = target.querySelector<HTMLInputElement>('.new-link-url');
		expect(url?.value).toContain('/s/tok-same');
		expect(toastShow).toHaveBeenCalledWith('Share link created', 'success');
	});

	it('a create from open #1 that succeeds after a reopen on another target shows no token there, and toasts', async () => {
		const createD = deferred<unknown>();
		apiMock.createItemShareLink.mockReturnValue(createD.promise);
		await render();
		createBtn().click();
		await settle();

		await reopen('ITEM-2');
		createD.resolve({ id: 'l1', token: 'tok-other', view_count: 0, created_at: '2026-11-27T00:00:00Z' });
		await settle();

		expect(target.querySelector('.new-link-url')).toBeNull();
		expect(target.textContent).not.toContain('tok-other');
		expect(toastShow).toHaveBeenCalledWith('Share link created', 'success');
	});

	it('a share from open #1 is not listed after a reopen on the same slug in another workspace', async () => {
		const shareD = deferred<unknown>();
		apiMock.createItemGrant.mockReturnValue(shareD.promise);
		await render();
		await startShare('old@example.com');

		props.open = false;
		flushSync();
		await settle();
		props.wsSlug = 'other-ws';
		props.open = true;
		flushSync();
		await settle();

		shareD.resolve(grantA);
		await settle();
		expect(grantsSection().textContent).not.toContain('old@example.com');
	});
});
