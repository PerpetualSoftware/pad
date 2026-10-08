import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { BuiltinStateResponse } from '$lib/types';

// BuiltinUpdateNotice (TASK-3462 U3b): the item pane's badge and review
// dialog for a built-in whose library text is newer. It renders nothing
// without an offer, accepts only on a press, sends the seq it previewed, and
// reads each refusal for what it is.

const state = vi.hoisted(() => ({
	get: [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>,
	update: [] as Array<{ body: unknown; resolve: (v: unknown) => void; reject: (e: unknown) => void }>,
}));
vi.mock('$lib/api/client', () => ({
	api: {
		builtins: {
			get: vi.fn(() => new Promise((resolve, reject) => state.get.push({ resolve, reject }))),
			update: vi.fn((_ws: string, _ref: string, body: unknown) =>
				new Promise((resolve, reject) => state.update.push({ body, resolve, reject }))),
		},
	},
}));

const auth = vi.hoisted(() => {
	let epoch = 0;
	const listeners = new Set<() => void>();
	return {
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		changeIdentity() {
			epoch++;
			for (const fn of listeners) fn();
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import BuiltinUpdateNotice from './BuiltinUpdateNotice.svelte';

function offer(st: BuiltinStateResponse['state'], extra: Partial<BuiltinStateResponse> = {}): BuiltinStateResponse {
	return {
		key: 'playbook/plan',
		kind: 'playbook',
		state: st,
		seq: 7,
		library: { content: 'library body', fields: { trigger: 'manual' } },
		seed: { content: 'seed body', fields: { trigger: 'manual' } },
		...extra,
	};
}

async function settle() {
	for (let i = 0; i < 4; i++) await Promise.resolve();
	flushSync();
}

function apiError(code: string): Error & { code: string } {
	return Object.assign(new Error(code), { code });
}

let cmp: ReturnType<typeof mount> | null = null;
let updated = 0;

function render(canEdit = true) {
	cmp = mount(BuiltinUpdateNotice, {
		target: document.body,
		props: {
			wsSlug: 'ws',
			itemRef: 'plan',
			seq: 7,
			currentContent: 'my body',
			currentFields: JSON.stringify({ trigger: 'on-release' }),
			canEdit,
			onUpdated: () => {
				updated++;
			},
		},
	});
	flushSync();
}

const badge = () => document.querySelector<HTMLButtonElement>('.builtin-badge');
const acceptBtn = () =>
	[...document.querySelectorAll<HTMLButtonElement>('.modal-footer button')].find((b) => /accept/i.test(b.textContent ?? ''));

beforeEach(() => {
	updated = 0;
	state.get.length = 0;
	state.update.length = 0;
});
afterEach(() => {
	if (cmp) unmount(cmp);
	cmp = null;
	document.body.innerHTML = '';
});

describe('BuiltinUpdateNotice', () => {
	it('renders nothing for a current item, or one made from no built-in', async () => {
		render();
		state.get[0]!.resolve(offer('current'));
		await settle();
		expect(badge()).toBeNull();
		if (cmp) unmount(cmp);
		document.body.innerHTML = '';
		render();
		state.get[0 + 1]!.reject(apiError('not_builtin'));
		await settle();
		expect(badge()).toBeNull();
	});

	it('labels each offer for what it is', async () => {
		for (const [st, label] of [
			['update_available', 'Update available'],
			['diverged', 'Library changed'],
			['unknown_origin', 'Library version differs'],
		] as const) {
			render();
			state.get[state.get.length - 1]!.resolve(offer(st));
			await settle();
			expect(badge()?.textContent?.trim()).toBe(label);
			if (cmp) unmount(cmp);
			cmp = null;
			document.body.innerHTML = '';
		}
	});

	it('accepts only on a press, sending the seq it previewed, then asks the pane to reload', async () => {
		render();
		state.get[0]!.resolve(offer('diverged'));
		await settle();
		expect(state.update).toHaveLength(0);
		badge()!.click();
		flushSync();
		acceptBtn()!.click();
		flushSync();
		expect(state.update).toHaveLength(1);
		expect(state.update[0]!.body).toEqual({ expected_seq: 7 });
		state.update[0]!.resolve({});
		await settle();
		expect(updated).toBe(1);
		expect(badge()).toBeNull();
	});

	it('names the field values accepting would replace', async () => {
		render();
		state.get[0]!.resolve(offer('update_available'));
		await settle();
		badge()!.click();
		flushSync();
		const list = document.querySelector('.field-changes')?.textContent ?? '';
		expect(list).toContain('trigger');
		expect(list).toContain('on-release');
		expect(list).toContain('manual');
	});

	it('on content_pending_flush, a second press discards the unsaved edits, and says so', async () => {
		render();
		state.get[0]!.resolve(offer('update_available'));
		await settle();
		badge()!.click();
		flushSync();
		acceptBtn()!.click();
		flushSync();
		state.update[0]!.reject(apiError('content_pending_flush'));
		await settle();
		expect(acceptBtn()!.textContent).toMatch(/discard unsaved edits/i);
		expect(updated).toBe(0);
		acceptBtn()!.click();
		flushSync();
		expect(state.update[1]!.body).toEqual({ expected_seq: 7, overwrite_pending_edits: true });
	});

	it('on update_conflict, refreshes the preview instead of updating', async () => {
		render();
		state.get[0]!.resolve(offer('update_available'));
		await settle();
		badge()!.click();
		flushSync();
		acceptBtn()!.click();
		flushSync();
		state.update[0]!.reject(apiError('update_conflict'));
		await settle();
		expect(updated).toBe(0);
		expect(state.get).toHaveLength(2);
		expect(document.querySelector('.error')?.textContent).toMatch(/changed since this preview/);
	});

	it('an update that settles after the identity moved commits nothing', async () => {
		render();
		state.get[0]!.resolve(offer('update_available'));
		await settle();
		badge()!.click();
		flushSync();
		acceptBtn()!.click();
		flushSync();
		auth.changeIdentity();
		state.update[0]!.resolve({});
		await settle();
		expect(updated).toBe(0);
	});

	it('offers no Accept to someone who cannot edit the item', async () => {
		render(false);
		state.get[0]!.resolve(offer('update_available'));
		await settle();
		badge()!.click();
		flushSync();
		expect(acceptBtn()).toBeUndefined();
		expect(document.querySelector('.readonly')).not.toBeNull();
	});
});
