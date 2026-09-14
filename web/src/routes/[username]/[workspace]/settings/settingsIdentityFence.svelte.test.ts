import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';


/**
 * BUG-3006 — the BEHAVIOURAL half. `settingsIdentityFence.test.ts` one directory
 * up owns the POPULATION (every async commit point is fenced, and an unknown
 * handler fails the build); this file owns the SEMANTICS: that a fence actually
 * stops the commit when the identity moves mid-flight.
 *
 * The two are separable on purpose. A source guard checks spellings, so it
 * cannot tell a fence comparing the right two values from one comparing the
 * wrong ones, or one placed after the commit it guards. A behavioural suite
 * covers only the handlers someone wrote a case for, so it cannot tell you a
 * fifteenth arrived unfenced. Neither is redundant with the other.
 *
 * THE MOVE EACH TEST MAKES: start a handler, hold its request open, flip the
 * signed-in identity with `authStore.clear()` (which bumps `identityEpoch`),
 * then resolve. The previous session's answer lands into a page whose user has
 * changed, which is exactly the window the item describes.
 *
 * ONE CONTROL PER CASE, because "nothing was committed" is satisfied just as
 * well by a handler that never ran, a button that was not found, or a mock that
 * was never called. Each test asserts the same sequence WITHOUT the identity
 * flip and sees the commit land.
 *
 * WHAT THIS FILE DOES NOT COVER, stated rather than implied (codex round 1
 * [Medium]). It drives four of the fourteen commit points — member removal, the
 * rename status timer, the Undo toast, and the collection-access revert — chosen
 * as one per TIMING CLASS plus the callback that outlives its page, not as a
 * sample of handlers. The remaining ten share a class with one of these and are
 * covered by the source guard's enumeration, which is the instrument that can
 * speak about all fourteen at once; a behavioural case per handler would be ten
 * more mounts measuring the same three shapes.
 *
 * The mocked store also moves ONLY the epoch, so the real identity-change
 * RELOAD path (BUG-2991/BUG-3005, keyed on the user id) never fires here. That
 * is deliberate — it is the mechanism that would otherwise tear the page down
 * before a commit could be attempted, and this suite is about what happens when
 * a commit IS attempted — but it means these tests say nothing about whether
 * that reload works. Its own suites own that.
 */

type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void };

function deferred<T>(): Deferred<T> {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

const meCalls: Array<(value: unknown) => void> = [];
/** Outstanding member-removal requests, one deferred per call. */
const removeCalls: Array<Deferred<unknown>> = [];
/** Outstanding workspace-name updates. */
const updateCalls: Array<Deferred<unknown>> = [];
/** Outstanding collection-access saves. */
const accessSaveCalls: Array<Deferred<unknown>> = [];

let restoreCalls = 0;

const toasts = vi.hoisted(() => [] as Array<{ message: string; action?: { label: string; onAction: () => void } }>);

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: {
		show: (message: string, _t?: string, _d?: number, _l?: string, action?: { label: string; onAction: () => void }) => {
			toasts.push({ message, action });
			return 'toast-id';
		},
		dismiss: () => {},
		get toasts() {
			return [];
		},
	},
	quietExternalToasts: () => false,
}));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
			me: vi.fn(() => new Promise((resolve) => { meCalls.push(resolve); })),
			list: vi.fn(async () => []),
			update: vi.fn(() => {
				const d = deferred<unknown>();
				updateCalls.push(d);
				return d.promise;
			}),
			delete: vi.fn(async () => ({})),
			restore: vi.fn(async () => { restoreCalls++; return { slug: 'ws', name: 'WS' }; }),
		},
		collections: { list: vi.fn(async () => []) },
		members: {
			list: vi.fn(async () => ({
				members: [{ user_id: 'u2', user_name: 'Bob', user_email: 'bob@example.com', role: 'editor' }],
				invitations: [],
			})),
			getMemberCollectionAccess: vi.fn(async () => ({ collection_access: 'all', collection_ids: [] })),
			setMemberCollectionAccess: vi.fn(() => {
				const d = deferred<unknown>();
				accessSaveCalls.push(d);
				return d.promise;
			}),
			remove: vi.fn(() => {
				const d = deferred<unknown>();
				removeCalls.push(d);
				return d.promise;
			}),
		},
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

const gotos = vi.hoisted(() => [] as string[]);
vi.mock('$app/navigation', () => ({
	goto: (url: string) => { gotos.push(url); return Promise.resolve(); },
}));

/**
 * The auth store is MOCKED rather than driven, following
 * `identityResetRound2.svelte.test.ts`. The real store only bumps its epoch
 * once an identity has been ESTABLISHED and then changes to a different one, so
 * a test that merely calls `clear()` on a store no session was ever loaded into
 * establishes the baseline and bumps nothing — the first version of this file
 * did exactly that, and its own guard on the bump is what caught it.
 *
 * Only the EPOCH is moved, and `userId` is deliberately held still. A real
 * identity change moves both, and the page's own reload-on-identity path
 * (BUG-2991) keys on the other one — leaving it alone isolates the fence under
 * test from the mechanism that would otherwise tear the page down before the
 * commit could be attempted.
 */
const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		get identityEpoch() { return epoch; },
		get userId() { return 'u1'; },
		bumpEpoch() { epoch++; },
		resetEpoch() { epoch = 0; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange() { return () => {}; },
		clear() {},
	};
});

vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };

async function resolveMe(index: number, value: unknown) {
	await waitFor(() => expect(meCalls.length).toBeGreaterThan(index));
	meCalls[index]!(value);
}

/** Move the signed-in identity on. */
function flipIdentity() {
	const before = auth.identityEpoch;
	auth.bumpEpoch();
	// The flip IS the instrument. If it ever stopped moving the epoch, every
	// assertion below would pass while measuring nothing at all — which is what
	// happened before this file mocked the store.
	expect(auth.identityEpoch).toBeGreaterThan(before);
}

describe('BUG-3006: settings commits are fenced on the signed-in identity', () => {
	beforeEach(() => {
		meCalls.length = 0;
		removeCalls.length = 0;
		updateCalls.length = 0;
		accessSaveCalls.length = 0;
		toasts.length = 0;
		gotos.length = 0;
		restoreCalls = 0;
		auth.resetEpoch();
		page.params = { username: 'dave', workspace: 'ws' };
		// THE TAB IS PERSISTED IN THE URL HASH. `selectTab` writes
		// `history.replaceState(null, '', '#<tab>')` and the page restores from
		// `window.location.hash` on mount — and jsdom keeps that hash for the
		// life of the file. So a test that opened Members leaves the NEXT test's
		// fresh render on Members, where the General tab's controls do not
		// exist: the failure reads as "the page did not render the control" and
		// has nothing to do with what is being tested.
		window.location.hash = '';
		// `confirm` gates the destructive handlers; the page is not what this
		// suite is testing, so it always says yes.
		vi.stubGlobal('confirm', () => true);
	});

	afterEach(() => {
		// UNMOUNTED EXPLICITLY. Without this the previous test's component stays
		// in the document, and `screen` queries resolve against IT while the
		// fresh render is still loading — so a tab assertion passes against the
		// old page and the element the new test wants is absent. That reads
		// exactly like the page failing to render the control, which is how an
		// hour could go into the wrong half of this file.
		cleanup();
		window.location.hash = '';
		vi.unstubAllGlobals();
		// Deliberately NO vi.resetModules() — see settingsPermissionFlicker's
		// note: a fresh module graph brings a second Svelte runtime and the
		// remount dies with effect_orphan.
	});

	/** Mount and answer the load's `/me` as owner. Leaves the General tab open. */
	async function mountAsOwner() {
		render(SettingsPage);
		await resolveMe(0, OWNER);
		// The owner-only tab set is the precondition for everything below: on a
		// non-owner render the Save button and the Remove button are not in the
		// document at all, and "nothing committed" would be true for a reason
		// that has nothing to do with identity.
		await waitFor(() => expect(screen.getByRole('tab', { name: /Members/ })).toBeTruthy());
	}

	/** Switch to the Members tab and wait for the seeded member to render. */
	async function openMembers() {
		screen.getByRole('tab', { name: /Members/ }).click();
		await waitFor(() => expect(screen.getByText('Bob')).toBeTruthy());
	}

	/**
	 * Type a new workspace name and press Save.
	 *
	 * The input is addressed by its id rather than by display value: `wsName` is
	 * populated from the workspace store, which is a module singleton shared
	 * across this file's worker, so its starting value is not something an
	 * individual test may assume. Setting `.value` and dispatching `input` is
	 * what drives Svelte 5's `bind:value`.
	 */
	async function renameWorkspace(next: string) {
		const input = document.querySelector('#ws-name') as HTMLInputElement | null;
		expect(input, 'the workspace-name input is not rendered — the owner gate or the tab changed').not.toBeNull();
		input!.value = next;
		input!.dispatchEvent(new Event('input', { bubbles: true }));
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(updateCalls.length).toBe(1));
	}

	it('CONTROL: a member removal that is not interrupted commits and reports', async () => {
		await mountAsOwner();
		await openMembers();
		screen.getByRole('button', { name: /Remove/ }).click();
		await waitFor(() => expect(removeCalls.length).toBe(1));
		removeCalls[0]!.resolve({});
		// Without this leg, the identity-change test below would pass against a
		// page where the button does nothing at all.
		await waitFor(() => expect(toasts.map((t) => t.message)).toContain('Removed Bob'));
		expect(screen.queryByText('Bob')).toBeNull();
	});

	it('discards a member removal whose answer lands after the identity changed', async () => {
		await mountAsOwner();
		await openMembers();
		screen.getByRole('button', { name: /Remove/ }).click();
		await waitFor(() => expect(removeCalls.length).toBe(1));

		flipIdentity();
		removeCalls[0]!.resolve({});
		await waitFor(() => expect(removeCalls.length).toBe(1));

		// NEITHER half commits: not the list write, and not the report. The
		// report is the half the server cannot bound — a toast naming another
		// workspace's member reaches whoever is signed in now even when the
		// request itself would be refused.
		expect(toasts.map((t) => t.message)).not.toContain('Removed Bob');
	});

	/**
	 * Delete the workspace and return the Undo action the toast was handed.
	 *
	 * This is the callback BUG-3006 calls the worst case, and it is the only one
	 * on the page that outlives its own page: `handleDeleteWorkspace` navigates
	 * to /console two lines after showing the toast, and the toast store is
	 * global, so the callback is still clickable in whatever the next user is
	 * looking at.
	 */
	async function deleteWorkspaceAndTakeUndo(): Promise<() => void> {
		screen.getByRole('tab', { name: /Danger Zone/ }).click();
		await waitFor(() => expect(screen.getByRole('button', { name: /Delete workspace/ })).toBeTruthy());
		screen.getByRole('button', { name: /Delete workspace/ }).click();
		const confirmInput = await waitFor(() => {
			const el = document.querySelector('input[placeholder*="slug"]') as HTMLInputElement | null;
			expect(el).not.toBeNull();
			return el!;
		});
		// The button is disabled until the typed slug matches, so this is not
		// ceremony — without it the click below is a no-op and the test would
		// assert against a toast that was never shown.
		confirmInput.value = 'ws';
		confirmInput.dispatchEvent(new Event('input', { bubbles: true }));
		await waitFor(() =>
			expect((screen.getByRole('button', { name: /Delete this workspace/ }) as HTMLButtonElement).disabled).toBe(false)
		);
		screen.getByRole('button', { name: /Delete this workspace/ }).click();

		const toast = await waitFor(() => {
			const t = toasts.find((entry) => entry.action?.label === 'Undo');
			expect(t, 'no Undo toast was shown, so there is no callback to test').toBeDefined();
			return t!;
		});
		return toast.action!.onAction;
	}

	it('CONTROL: the Undo toast restores the workspace when the same user clicks it', async () => {
		await mountAsOwner();
		const undo = await deleteWorkspaceAndTakeUndo();
		undo();
		await waitFor(() => expect(restoreCalls).toBe(1));
	});

	it('refuses the Undo toast when a different user clicks it', async () => {
		await mountAsOwner();
		const undo = await deleteWorkspaceAndTakeUndo();

		// The identity moves AFTER the toast is showing and BEFORE the click —
		// the window the toast's 12-second lifetime opens. The previous user's
		// workspace name is already on screen; what must not happen is the
		// restore, and the navigation into that workspace's owner URL behind it.
		flipIdentity();
		undo();
		await Promise.resolve();
		await Promise.resolve();
		expect(restoreCalls).toBe(0);
		expect(gotos.filter((u) => u.includes('/dave/ws'))).toEqual([]);
	});

	/**
	 * Resolve the rename under FAKE TIMERS and return once the status has
	 * committed and the 2s timer is on the fake clock.
	 *
	 * The clock is installed BEFORE the resolve, and that ordering is the whole
	 * point: a `setTimeout` scheduled while real timers are live is not on the
	 * fake clock at all, so `advanceTimersByTime` moves nothing and the
	 * assertion reads a status that was never going to change. The first version
	 * installed the clock afterwards, and its control leg is what caught it.
	 *
	 * `waitFor` cannot be used past this point — it polls on a timer — so the
	 * settle is an explicit microtask drain plus a Svelte flush.
	 */
	async function resolveRenameOnFakeClock() {
		vi.useFakeTimers();
		updateCalls[0]!.resolve({ id: 'ws1', slug: 'ws', name: 'Renamed', context: {} });
		for (let i = 0; i < 8; i++) await Promise.resolve();
		flushSync();
		// The PRECONDITION for both timer legs: 'Saved' on screen proves the
		// status committed and therefore that the timer was scheduled.
		expect(screen.queryByText(/^Saved$/)).not.toBeNull();
	}

	it('discards the deferred status timer when the identity changes during its two seconds', async () => {
		// THE ORDER HERE IS THE TEST, and the first version got it wrong in a way
		// that looked right (codex round 1 [Medium]): it flipped the identity
		// BEFORE resolving the update, so the await-site fence returned early and
		// the timer was never scheduled at all. It passed, and it measured the
		// await fence a second time rather than the timer.
		//
		// The timer's own window is: the request comes back under the SAME
		// identity, the status commits, the timer is scheduled — and only then
		// does the identity move, with two seconds still on the clock.
		await mountAsOwner();
		await renameWorkspace('Renamed');
		try {
			await resolveRenameOnFakeClock();

			flipIdentity();
			vi.advanceTimersByTime(2500);
			flushSync();
			// The timer fires under the NEW identity and must not write, so the
			// status stays where the previous user's page left it rather than
			// being reset by their leftover timer.
			expect(screen.queryByText(/^Saved$/)).not.toBeNull();
		} finally {
			vi.useRealTimers();
		}
	});

	it('CONTROL: the deferred timer DOES clear the status when the identity holds', async () => {
		// The counterfactual. Without it, "Saved is still there" is satisfied by
		// a timer that never ran for any reason at all — which is exactly what
		// was happening while the fake clock was installed too late.
		await mountAsOwner();
		await renameWorkspace('Renamed');
		try {
			await resolveRenameOnFakeClock();

			vi.advanceTimersByTime(2500);
			flushSync();
			expect(screen.queryByText(/^Saved$/)).toBeNull();
		} finally {
			vi.useRealTimers();
		}
	});

	/** The member's collection-visibility select, once the access panel is open. */
	function accessModeSelect(): HTMLSelectElement {
		const el = document.querySelector('select[id^="access-mode-"]') as HTMLSelectElement | null;
		expect(el, 'the access panel is not open — the Manage access click or the owner gate changed').not.toBeNull();
		return el!;
	}

	function setAccessMode(next: 'all' | 'specific') {
		const select = accessModeSelect();
		select.value = next;
		select.dispatchEvent(new Event('change', { bubbles: true }));
	}

	/**
	 * Drive the access panel into the one state where the REVERT is observable,
	 * and leave the save request open.
	 *
	 * WHY THE SECOND CHANGE IS THE WHOLE SETUP (codex round 2). The revert
	 * restores `prevMode`/`prevIds`, captured before the await — so if nothing
	 * moves them afterwards, the revert writes the values already on screen and
	 * is invisible. The first version of these tests saved from the panel's
	 * default state and asserted only the toast, which an unfenced revert would
	 * have passed unchanged.
	 *
	 * So: choose `specific`, start the save (that is what `prevMode` captures),
	 * then change the panel to `all` while the request is in flight. A revert
	 * now STOMPS the visible state back to `specific`, which is exactly the
	 * write that must not happen under a changed identity.
	 */
	async function saveAccessThenChangeItAgain() {
		screen.getByRole('button', { name: /Manage access/ }).click();
		await waitFor(() => expect(accessModeSelect()).toBeTruthy());
		setAccessMode('specific');
		await waitFor(() => expect(accessModeSelect().value).toBe('specific'));

		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(accessSaveCalls.length).toBe(1));

		setAccessMode('all');
		await waitFor(() => expect(accessModeSelect().value).toBe('all'));
	}

	it('CONTROL: a failed access save reverts the panel when the identity holds', async () => {
		await mountAsOwner();
		await openMembers();
		await saveAccessThenChangeItAgain();

		accessSaveCalls[0]!.reject(new Error('nope'));
		// The revert stomps the in-flight change back to what was captured. This
		// is the leg that proves the revert is OBSERVABLE at all; without it the
		// identity leg below asserts the absence of something that never happens.
		await waitFor(() => expect(accessModeSelect().value).toBe('specific'));
		expect(toasts.map((t) => t.message)).toContain('Failed to update collection access');
	});

	it('discards the access-save REVERT when the identity changed during the request', async () => {
		await mountAsOwner();
		await openMembers();
		await saveAccessThenChangeItAgain();

		flipIdentity();
		accessSaveCalls[0]!.reject(new Error('nope'));
		await Promise.resolve();
		await Promise.resolve();
		await tick();

		// The panel keeps what is on screen NOW rather than the values captured
		// under the previous identity — the revert is the worse half of this
		// handler precisely because those values are one session's view of
		// another member's access rights.
		expect(accessModeSelect().value).toBe('all');
		expect(toasts.map((t) => t.message)).not.toContain('Failed to update collection access');
	});

	it('CONTROL: the status reaches Saved when the identity holds', async () => {
		// Without this leg the test above passes against a page where the Save
		// button never sets the status at all.
		await mountAsOwner();
		await renameWorkspace('Renamed');

		updateCalls[0]!.resolve({ id: 'ws1', slug: 'ws', name: 'Renamed', context: {} });
		await waitFor(() => expect(screen.queryByText(/^Saved$/)).not.toBeNull());
	});
});
