import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * BUG-3084 surface 2 — the BEHAVIOURAL half. `rolesIdentityFence.test.ts`
 * beside this file owns the POPULATION; this owns the SEMANTICS.
 *
 * THE MOVE EACH TEST MAKES: start a handler, hold its request open, bump the
 * identity epoch, then resolve, and assert the commit did not land.
 *
 * ONE CONTROL PER CASE, because "nothing was committed" is satisfied just as
 * well by a handler that never ran or a mount that failed.
 *
 * WHAT THIS FILE DRIVES, and what it does not. This surface has ONE timing
 * class — await, then commit — so there is no timer case, no subscription case
 * and no outlives-the-page case to write, unlike surface 1. What it does have
 * that surface 1 did not is a handler that COMPOSES A WRITE from state captured
 * under the previous identity, and that gets its own case because the fence
 * there has to precede the REQUEST rather than the commit after it. Two of the
 * six handlers are driven; the other four share `saveRole`'s shape and are the
 * source guard's to speak for.
 *
 * The fake auth store moves ONLY the epoch, so the real identity-change reload
 * never fires here — deliberate, since that reload is what would otherwise tear
 * the page down before a commit could be attempted, and this suite is about
 * what happens when one IS attempted.
 */

const toasts = vi.hoisted(() => [] as Array<{ message: string }>);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: {
		show: (message: string) => { toasts.push({ message }); return 'id'; },
		dismiss: () => {},
		get toasts() { return []; },
	},
	quietExternalToasts: () => false,
}));

const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		get identityEpoch() { return epoch; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		get session() { return { user: { id: 'u1' } }; },
		bumpEpoch() { epoch++; },
		resetEpoch() { epoch = 0; },
		identityFence() { const c = epoch; return () => epoch === c; },
		// A REAL registry, not a no-op (BUG-3084 checkpoint 13). The reload leg
		// below has to drive the page's own identity-change listener, because
		// the window it is about only exists WHILE that reload is in flight.
		listeners: [] as Array<() => void>,
		onIdentityChange(cb: () => void) {
			this.listeners.push(cb);
			return () => { this.listeners = this.listeners.filter((l) => l !== cb); };
		},
		notify() { for (const l of [...this.listeners]) l(); },
		clear() {},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

const roleUpdates = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>);
const itemUpdates = vi.hoisted(() => [] as Array<{ args: unknown[]; resolve: (v: unknown) => void }>);
const boardCalls = vi.hoisted(() => [] as Array<(v: unknown) => void>);
// Which user `/auth/session` answers with. The resume leg flips it so the
// reload lands under a DIFFERENT identity than the mount did — without that,
// "the page works again" and "the page never changed hands" are the same
// observation (CONVE-12).
const sessionUser = vi.hoisted(() => ({ id: 'u1' }));

vi.mock('$lib/api/client', () => ({
	api: {
		agentRoles: {
			board: vi.fn(() => new Promise((resolve) => { boardCalls.push(resolve as (v: unknown) => void); })),
			update: vi.fn(() => new Promise((resolve, reject) => {
				roleUpdates.push({ resolve: resolve as (v: unknown) => void, reject });
			})),
			create: vi.fn(async () => ({})),
			delete: vi.fn(async () => ({})),
			reorder: vi.fn(async () => ({})),
			reorderLanes: vi.fn(async () => ({})),
		},
		items: {
			create: vi.fn(async () => ({})),
			update: vi.fn((...args: unknown[]) => new Promise((resolve) => {
				itemUpdates.push({ args, resolve: resolve as (v: unknown) => void });
			})),
		},
		auth: { session: vi.fn(async () => ({ authenticated: true, user: { id: sessionUser.id } })) },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get isOwner() { return true; },
		get currentRole() { return 'owner'; },
		get current() { return { id: 'ws1', slug: 'ws', name: 'WS' }; },
		get currentMembership() { return { role: 'owner' }; },
		canEditCollection: () => true,
		setCurrent: vi.fn(async () => {}),
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { get collections() { return []; }, loadCollections: vi.fn(async () => {}) },
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		registerCollectionSearch: () => {},
		unregisterCollectionSearch: () => {},
		onNavigate: () => () => {},
	},
}));
vi.mock('svelte-dnd-action', () => ({
	dndzone: () => ({ destroy: () => {} }),
	TRIGGERS: { DROPPED_INTO_ZONE: 'droppedIntoZone' },
	SHADOW_ITEM_MARKER_PROPERTY_NAME: '__dndShadow',
}));

import { api } from '$lib/api/client';
import { page } from '$app/state';
import RolesPage from './+page.svelte';

const ROLE = { id: 'r1', name: 'Implementer', slug: 'implementer', icon: '🔨', description: '', tools: '' };
const ITEM = {
	id: 'i1', slug: 'i1', title: 'Row', item_number: 1, collection_slug: 'tasks',
	fields: '{}', tags: '[]', agent_role_id: null, assigned_user_id: null,
};

async function mountPage() {
	toasts.length = 0;
	sessionUser.id = 'u1';
	roleUpdates.length = 0;
	itemUpdates.length = 0;
	boardCalls.length = 0;
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/roles');
	const r = render(RolesPage);
	const resolveBoard = await waitFor(() => {
		if (boardCalls.length === 0) throw new Error('no board fetch yet');
		return boardCalls[0]!;
	});
	resolveBoard({
		lanes: [
			{ role: null, items: [ITEM] },
			{ role: ROLE, items: [] },
		],
	});
	await waitFor(() => {
		if (!document.querySelector('[data-lane-key]') && !document.body.textContent?.includes('Implementer')) {
			throw new Error('board not rendered yet');
		}
	});
	return r;
}

function flipIdentity(): void {
	const before = auth.identityEpoch;
	auth.bumpEpoch();
	if (auth.identityEpoch === before) {
		throw new Error('the identity epoch did not move — this test would pass against an unfenced page');
	}
}

describe('the roles board stops a commit when the identity moves mid-flight', () => {
	beforeEach(() => auth.resetEpoch());
	afterEach(cleanup);

	// ── The distinctive case: a WRITE composed from previous-identity state ──
	//
	// `handleDndFinalize` builds `update.assigned_user_id = currentUserId`,
	// where `currentUserId` came from the previous session's `/auth/session`.
	// The fence therefore has to precede the REQUEST, not the commit after it —
	// a check placed after the await still lets the wrong assignee reach the
	// database, and the server cannot refuse it because assigning to another
	// user is a legitimate operation.
	//
	// Driven by dispatching the `finalize` event the dnd action would dispatch.
	// The action itself is stubbed: its drag machinery is not the subject, and
	// the handler is reached through the same DOM event either way.

	function dropIntoRoleLane(): void {
		const zones = document.querySelectorAll('.lane-items');
		expect(zones.length, 'the lane elements did not render — nothing to drop onto').toBeGreaterThan(1);
		// zones[0] is the unassigned lane (where ITEM starts); zones[1] is the
		// role lane it is dragged into.
		zones[1]!.dispatchEvent(
			new CustomEvent('finalize', {
				detail: { items: [{ ...ITEM }], info: { id: ITEM.id, trigger: 'droppedIntoZone' } },
			})
		);
	}

	it('CONTROL: a drop into a role lane issues the assignee write', async () => {
		await mountPage();
		dropIntoRoleLane();
		await waitFor(() => {
			expect(
				itemUpdates.length,
				'the control issued no write — the leg below would measure nothing'
			).toBe(1);
		});
		const [, , update] = itemUpdates[0]!.args as [string, string, Record<string, unknown>];
		expect(update.agent_role_id).toBe(ROLE.id);
		expect(update.assigned_user_id, 'the control did not carry the assignee this case is about').toBe('u1');
	});

	it('a drop after an identity move never issues the write at all', async () => {
		await mountPage();
		flipIdentity();
		dropIntoRoleLane();
		await tick();
		await tick();
		expect(
			itemUpdates.length,
			"the previous session's user id was sent as the assignee — a wrong row, not a wrong screen"
		).toBe(0);
	});

	it('a drop whose write is IN FLIGHT when the identity moves commits nothing', async () => {
		// The other half, and the one an entry capture DOES answer: here the
		// drag starts legitimately and the identity moves while the request is
		// open. The leg above covers the opposite order — a drag that starts
		// AFTER the change, which every entry capture passes because it IS the
		// current epoch. Two orders, two mechanisms; neither covers the other,
		// and the page needed both fences before both legs passed.
		await mountPage();
		dropIntoRoleLane();
		const write = await waitFor(() => {
			if (itemUpdates.length === 0) throw new Error('no write yet');
			return itemUpdates[0]!;
		});
		// OBSERVED AT THE SORT-ORDER REORDER, not at a board reload. The first
		// version of this leg asserted the board was not re-fetched — which the
		// success path never does, fence or no fence, so it PASSED against the
		// unfixed page and measured nothing. The counterfactual run is what said
		// so; reading the leg would not have.
		const reordersBefore = vi.mocked(api.agentRoles.reorder).mock.calls.length;
		flipIdentity();
		write.resolve({});
		await tick();
		await tick();
		expect(
			vi.mocked(api.agentRoles.reorder).mock.calls.length,
			"the previous session's drag persisted a sort order under the new identity"
		).toBe(reordersBefore);
	});

	it('CONTROL: a drop whose write completes under a held identity DOES persist the sort order', async () => {
		await mountPage();
		dropIntoRoleLane();
		const write = await waitFor(() => {
			if (itemUpdates.length === 0) throw new Error('no write yet');
			return itemUpdates[0]!;
		});
		const reordersBefore = vi.mocked(api.agentRoles.reorder).mock.calls.length;
		write.resolve({});
		await waitFor(() => {
			expect(
				vi.mocked(api.agentRoles.reorder).mock.calls.length,
				'the control did not persist — the leg above would measure nothing'
			).toBe(reordersBefore + 1);
		});
	});

	// ── The RECOVERY window (BUG-3084 checkpoint 13) ──────────────────────
	//
	// The other two drag legs ask whether a fence REFUSES. This one asks what
	// the fence VOUCHES FOR while the recovery that #1374 added is in flight.
	//
	// `loadData` re-stamps `identityEpochAtLoad` BEFORE its await, so from the
	// moment the identity-change reload starts until it lands, `pageIdentityHeld()`
	// is TRUE while `currentUserId` still holds the PREVIOUS session's id — the
	// one `handleDndFinalize` stamps into `assigned_user_id`. If the board is
	// still reachable in that window, the recovery reintroduces the wrong-row
	// write the whole surface exists to prevent.
	//
	// This leg is written to be able to say EITHER answer. It reports what the
	// DOM actually offers during the window rather than assuming the board is
	// there, because "no write happened" is satisfied just as well by a board
	// that was swapped for a loading skeleton — a different mechanism with the
	// same end state (CONVE-12).

	it('the identity-change reload leaves no window in which a drag carries the previous assignee', async () => {
		await mountPage();
		const boardsBeforeReload = boardCalls.length;

		flipIdentity();
		auth.notify();
		await tick();
		await tick();

		// The reload IS in flight: its board request was issued and is unresolved.
		expect(
			boardCalls.length,
			'the identity-change listener did not re-load — this leg would measure nothing'
		).toBe(boardsBeforeReload + 1);

		const zonesDuringReload = document.querySelectorAll('.lane-items').length;
		const writesBefore = itemUpdates.length;

		if (zonesDuringReload > 1) {
			// The board is still on screen mid-reload: the window is REACHABLE,
			// so drive it and assert the stale assignee does not reach the wire.
			dropIntoRoleLane();
			await tick();
			await tick();
			const stale = itemUpdates
				.slice(writesBefore)
				.map((w) => (w.args[2] as Record<string, unknown>)?.assigned_user_id);
			expect(
				stale.filter((id) => id === 'u1'),
				"a drag during the identity-change reload persisted the previous session's user id as the assignee"
			).toEqual([]);
		} else {
			// The board is NOT reachable during the reload — the markup's
			// `{#if loading}` branch replaced it. That is the mechanism that
			// closes this window, and it is recorded HERE so a future change
			// that renders the board while loading (keeping it on screen is an
			// obvious UX improvement) fails this leg instead of silently
			// re-opening the hole.
			expect(
				document.body.textContent,
				'the board vanished during the reload but the page is not in its loading state — neither mechanism is holding this window shut'
			).not.toContain('Implementer');
		}
	});

	// ── DID IT EVER RESUME? (BUG-3084 checkpoint 11) ──────────────────────
	//
	// The counterpart every other leg in this file is missing by construction.
	// They all ask "did it refuse?", and an instrument set that only ever asks
	// that can only measure UNDER-protection — the failure mode on the other
	// side, a fence that latches shut and never lifts, is invisible to all of
	// them. That is precisely what shipped in #1372 through four codex rounds
	// and a 10/10 matrix.
	//
	// So: move the identity, let the page recover, and then KEEP USING IT.

	it('recovers after an identity change and writes under the NEW identity', async () => {
		await mountPage();

		flipIdentity();
		sessionUser.id = 'u2';
		auth.notify();
		await tick();

		const reload = await waitFor(() => {
			if (boardCalls.length < 2) throw new Error('the identity change did not trigger a reload');
			return boardCalls[1]!;
		});
		reload({ lanes: [{ role: null, items: [ITEM] }, { role: ROLE, items: [] }] });

		await waitFor(() => {
			if (document.querySelectorAll('.lane-items').length < 2) {
				throw new Error('the board never came back after the identity change');
			}
		});

		const writesBefore = itemUpdates.length;
		dropIntoRoleLane();

		const write = await waitFor(() => {
			if (itemUpdates.length <= writesBefore) {
				throw new Error(
					'the board is rendered but a drag commits nothing — the fence latched shut and never lifted'
				);
			}
			return itemUpdates[itemUpdates.length - 1]!;
		});

		const [, , update] = write.args as [string, string, Record<string, unknown>];
		expect(update.agent_role_id).toBe(ROLE.id);
		expect(
			update.assigned_user_id,
			"the page resumed but under the PREVIOUS user's id — recovering is not the same as recovering correctly"
		).toBe('u2');
	});

	it('a drag interrupted by an identity change does not pin the board for ever', async () => {
		// THE LATCH (codex round 3 [P1]), and the reason this leg exists at all
		// is that every other one in this file asks "did it refuse?".
		//
		// `isDragging` disables the $effect that repopulates `laneData`. The
		// `{#if loading}` branch destroys the dnd zones without dispatching
		// `finalize`, so a drag in progress when the identity moves leaves
		// `isDragging` true with no handler left to clear it — and the board
		// then never repopulates, showing the previous identity's lane data for
		// ever. Same failure mode as #1372, in a different variable.
		await mountPage();

		// Start a drag: `consider` is what the dnd action dispatches first, and
		// it is where `isDragging` is set.
		const zones = document.querySelectorAll('.lane-items');
		expect(zones.length, 'the lanes did not render').toBeGreaterThan(1);
		zones[0]!.dispatchEvent(
			new CustomEvent('consider', {
				detail: { items: [{ ...ITEM }], info: { id: ITEM.id, trigger: 'draggedEntered' } },
			})
		);
		await tick();

		flipIdentity();
		sessionUser.id = 'u2';
		auth.notify();
		await tick();

		const reload = await waitFor(() => {
			if (boardCalls.length < 2) throw new Error('the identity change did not trigger a reload');
			return boardCalls[1]!;
		});
		reload({ lanes: [{ role: null, items: [ITEM] }, { role: ROLE, items: [] }] });

		// The board must come back AND be usable. "Rendered" alone is not
		// enough: the lanes render from `orderedLanes`, while the drop handler
		// reads `laneData`, which is exactly what the latch starves.
		await waitFor(() => {
			if (document.querySelectorAll('.lane-items').length < 2) {
				throw new Error('the board never came back after the interrupted drag');
			}
		});

		const writesBefore = itemUpdates.length;
		dropIntoRoleLane();
		const write = await waitFor(() => {
			if (itemUpdates.length <= writesBefore) {
				throw new Error(
					'the board is rendered but a drop commits nothing — isDragging latched true and the ' +
						'laneData sync never ran again'
				);
			}
			return itemUpdates[itemUpdates.length - 1]!;
		});
		const [, , update] = write.args as [string, string, Record<string, unknown>];
		expect(
			update.assigned_user_id,
			'the board resumed under the previous user id'
		).toBe('u2');
	});

	// ── The ordinary class: await, then commit ────────────────────────────

	async function openEditModalForRole(): Promise<void> {
		const edit = document.querySelector('.lane-edit-btn') as HTMLButtonElement | null;
		expect(edit, 'the edit-role button did not render').not.toBeNull();
		edit!.click();
		await tick();
	}

	function clickSave(): void {
		const save = [...document.querySelectorAll('button')].find(
			(b) => b.textContent?.trim() === 'Save'
		) as HTMLButtonElement | undefined;
		expect(save, 'the Save button did not render').toBeDefined();
		save!.click();
	}

	it('CONTROL: saving a role closes the modal and reloads the board', async () => {
		await mountPage();
		await openEditModalForRole();
		clickSave();
		const d = await waitFor(() => {
			if (roleUpdates.length === 0) throw new Error('no role update yet');
			return roleUpdates[0]!;
		});
		const boardsBefore = vi.mocked(api.agentRoles.board).mock.calls.length;
		d.resolve({});
		await waitFor(() => {
			expect(
				vi.mocked(api.agentRoles.board).mock.calls.length,
				'the control did not reload — the leg below would measure nothing'
			).toBe(boardsBefore + 1);
		});
	});

	it('a role save CLICKED after an identity move never issues the write', async () => {
		// The other order, and the one an entry capture cannot see (codex round
		// 2 [P1]). The leg below opens the modal and clicks BEFORE the change,
		// so the capture catches it. Here the form is already open when the
		// identity moves and the click comes after — the capture is the current
		// epoch and passes, while `editingRoleId` is still a role id from the
		// PREVIOUS identity's board.
		await mountPage();
		await openEditModalForRole();

		flipIdentity();
		clickSave();
		await tick();
		await tick();

		expect(
			roleUpdates.length,
			"a role id taken from the previous identity's board was written under the new identity"
		).toBe(0);
	});

	it('a role save resolving after an identity move neither closes nor reloads', async () => {
		await mountPage();
		await openEditModalForRole();
		clickSave();
		const d = await waitFor(() => {
			if (roleUpdates.length === 0) throw new Error('no role update yet');
			return roleUpdates[0]!;
		});
		const boardsBefore = vi.mocked(api.agentRoles.board).mock.calls.length;
		flipIdentity();
		d.resolve({});
		await tick();
		await tick();
		expect(
			vi.mocked(api.agentRoles.board).mock.calls.length,
			"the previous session's save repainted the board for whoever is signed in now"
		).toBe(boardsBefore);
	});
});
