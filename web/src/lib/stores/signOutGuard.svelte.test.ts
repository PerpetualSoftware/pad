import { describe, it, expect, vi, beforeEach } from 'vitest';

vi.mock('./auth.svelte', () => ({
	authStore: { onIdentityChange: () => () => {}, userId: 'u1' },
}));

const { confirmDialog } = await import('./confirmDialog.svelte');
const { registerSignOutGuard, confirmSignOut, signOutDiscarding, signOutFailed } = await import('./signOutGuard.svelte');
const { keepRefusedRawDraft, readRefusedRawDraft } = await import('$lib/items/refusedRawDraft');

// BUG-3571: signing out asks first while an open item holds at-risk edits;
// a confirm discards them and keeps nothing; Cancel (the default) keeps all.
describe('BUG-3571: confirmSignOut', () => {
	beforeEach(() => {
		signOutFailed();
		confirmDialog.abandonAll();
	});

	it('nothing at risk: no question, and signing out goes ahead', async () => {
		const discard = vi.fn();
		const off = registerSignOutGuard({ atRisk: () => false, discard });
		await expect(confirmSignOut()).resolves.toBe(true);
		expect(confirmDialog.active).toBeNull();
		expect(discard).not.toHaveBeenCalled();
		expect(signOutDiscarding()).toBe(false);
		off();
	});

	it('at risk, Cancel: stays signed in and discards nothing', async () => {
		const discard = vi.fn();
		const off = registerSignOutGuard({ atRisk: () => true, discard });
		const answer = confirmSignOut();
		expect(confirmDialog.active?.confirmLabel).toBe('Sign out and discard');
		confirmDialog.cancel();
		await expect(answer).resolves.toBe(false);
		expect(discard).not.toHaveBeenCalled();
		expect(signOutDiscarding()).toBe(false);
		off();
	});

	it('at risk, confirm: every at-risk item discards, and items stand down', async () => {
		const risky = vi.fn();
		const clean = vi.fn();
		const offA = registerSignOutGuard({ atRisk: () => true, discard: risky });
		const offB = registerSignOutGuard({ atRisk: () => false, discard: clean });
		const answer = confirmSignOut();
		confirmDialog.confirm();
		await expect(answer).resolves.toBe(true);
		expect(risky).toHaveBeenCalledOnce();
		expect(clean).not.toHaveBeenCalled();
		expect(signOutDiscarding()).toBe(true);
		offA();
		offB();
	});

	it('a failed logout resets the discard, so items save again', async () => {
		const off = registerSignOutGuard({ atRisk: () => true, discard: () => {} });
		const answer = confirmSignOut();
		confirmDialog.confirm();
		await answer;
		signOutFailed();
		expect(signOutDiscarding()).toBe(false);
		off();
	});

	it('an unregistered item is no longer asked about', async () => {
		const off = registerSignOutGuard({ atRisk: () => true, discard: () => {} });
		off();
		await expect(confirmSignOut()).resolves.toBe(true);
		expect(confirmDialog.active).toBeNull();
	});
});

// BUG-3571 part 2: an explicit sign-out purges every draft this browser kept
// for the signed-in user, on every item; a cancelled one purges nothing.
describe('BUG-3571 part 2: sign-out purges the kept drafts', () => {
	beforeEach(() => {
		signOutFailed();
		confirmDialog.abandonAll();
		localStorage.clear();
		keepRefusedRawDraft('u1', 'i1', 'mine');
		keepRefusedRawDraft('u1', 'i2', 'mine too');
		keepRefusedRawDraft('u2', 'i1', 'theirs');
	});

	it('nothing at risk: purged without a question', async () => {
		await expect(confirmSignOut()).resolves.toBe(true);
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
		expect(readRefusedRawDraft('u1', 'i2')).toBeNull();
		expect(readRefusedRawDraft('u2', 'i1')?.markdown).toBe('theirs');
	});

	it('at risk, confirm: purged', async () => {
		const off = registerSignOutGuard({ atRisk: () => true, discard: () => {} });
		const answer = confirmSignOut();
		confirmDialog.confirm();
		await answer;
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
		expect(readRefusedRawDraft('u2', 'i1')?.markdown).toBe('theirs');
		off();
	});

	it('at risk, Cancel: nothing purged', async () => {
		const off = registerSignOutGuard({ atRisk: () => true, discard: () => {} });
		const answer = confirmSignOut();
		confirmDialog.cancel();
		await answer;
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('mine');
		off();
	});
});
