import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * BUG-3084 surface 3 — the BEHAVIOURAL half. `libraryIdentityFence.test.ts`
 * beside this file owns the POPULATION; this owns the SEMANTICS.
 *
 * THE MOVE EACH TEST MAKES: start a handler, hold its request open, move the
 * identity, then resolve, and assert the commit did not land.
 *
 * ONE CONTROL PER CASE, because "nothing was committed" is satisfied just as
 * well by a handler that never ran or a mount that failed.
 *
 * WHAT THIS SURFACE HAS that the earlier ones did not: a DEFERRED TIMER class.
 * Four `setTimeout(…, 3000)` callbacks clear the toast long after their handler
 * returned, and they are driven here with fake timers rather than reasoned
 * about — the filed survey missed all four, so they get the sharpest legs.
 *
 * And the leg every instrument set in this family was missing until #1374:
 * one case that moves the identity and then KEEPS USING the page. An
 * instrument set that only ever asks "did it refuse?" can only measure
 * under-protection, and the failure on the other side — a fence that latches
 * shut and never lifts — is invisible to all of it.
 */

const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		get identityEpoch() { return epoch; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		bumpEpoch() { epoch++; },
		resetEpoch() { epoch = 0; },
		identityFence() { const c = epoch; return () => epoch === c; },
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

const libraryCalls = vi.hoisted(() => [] as Array<(v: unknown) => void>);
const activateCalls = vi.hoisted(
	() => [] as Array<{ args: unknown[]; resolve: (v: unknown) => void; reject: (e: unknown) => void }>
);
// `activatePlaybook` gets its own driven legs rather than riding on the
// convention handler's (codex round 1 [P2]). The two are near-identical, which
// is exactly why source-text assertions alone would not notice one of them
// going inert.
const playbookCalls = vi.hoisted(
	() => [] as Array<{ args: unknown[]; resolve: (v: unknown) => void; reject: (e: unknown) => void }>
);

vi.mock('$lib/api/client', () => ({
	api: {
		library: {
			get: vi.fn(() => new Promise((resolve) => { libraryCalls.push(resolve as (v: unknown) => void); })),
			getPlaybooks: vi.fn(async () => ({ categories: [{ name: 'agent-workflows', playbooks: [PLAYBOOK] }] })),
			activate: vi.fn((...args: unknown[]) => new Promise((resolve, reject) => {
				activateCalls.push({ args, resolve: resolve as (v: unknown) => void, reject });
			})),
			activatePlaybook: vi.fn((...args: unknown[]) => new Promise((resolve, reject) => {
				playbookCalls.push({ args, resolve: resolve as (v: unknown) => void, reject });
			})),
		},
		items: { listByCollection: vi.fn(async () => []) },
	},
}));

vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { api } from '$lib/api/client';
import { page } from '$app/state';
import LibraryPage from './+page.svelte';

const PLAYBOOK = {
	title: 'Ship',
	description: 'd',
	content: 'body',
	category: 'agent-workflows',
	trigger: 'manual',
	scope: 'all',
	invocation_slug: 'ship',
	arguments: [],
};

const CONVENTION = {
	title: 'Use conventional commits',
	description: 'd',
	content: 'body',
	category: 'git',
	trigger: 'on-commit',
	priority: 'should',
	surfaces: ['all'],
};

async function mountPage() {
	libraryCalls.length = 0;
	activateCalls.length = 0;
	playbookCalls.length = 0;
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/library');
	const r = render(LibraryPage);
	const resolveLibrary = await waitFor(() => {
		if (libraryCalls.length === 0) throw new Error('no library fetch yet');
		return libraryCalls[0]!;
	});
	resolveLibrary({ categories: [{ name: 'git', conventions: [CONVENTION] }] });
	await waitFor(() => {
		if (!document.querySelector('.activate-btn')) throw new Error('library not rendered yet');
	});
	return r;
}

function clickActivate(): void {
	const btn = document.querySelector('.activate-btn') as HTMLButtonElement | null;
	expect(btn, 'the activate button did not render — nothing to click').not.toBeNull();
	btn!.click();
}

function flipIdentity(): void {
	const before = auth.identityEpoch;
	auth.bumpEpoch();
	if (auth.identityEpoch === before) {
		throw new Error('the identity epoch did not move — this test would pass against an unfenced page');
	}
}

describe('the library page stops a commit when the identity moves mid-flight', () => {
	beforeEach(() => { auth.resetEpoch(); auth.listeners.length = 0; });
	afterEach(() => { cleanup(); vi.useRealTimers(); });

	it('CONTROL: activating a convention issues the write and shows the toast', async () => {
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		call.resolve({});
		await waitFor(() => {
			expect(
				document.querySelector('.toast')?.textContent,
				'the control showed no toast — the legs below would measure nothing'
			).toContain('Activated');
		});
	});

	it('a click AFTER an identity move never issues the activation at all', async () => {
		// The order an entry capture cannot see: the click IS the current epoch,
		// so every capture passes, while the `convention` it carries came from
		// the list the PREVIOUS identity loaded.
		await mountPage();
		flipIdentity();
		clickActivate();
		await tick();
		await tick();
		expect(
			activateCalls.length,
			"a convention chosen from the previous identity's library was activated into the " +
				'workspace on screen now'
		).toBe(0);
	});

	it('an activation IN FLIGHT when the identity moves commits nothing', async () => {
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		flipIdentity();
		call.resolve({});
		await tick();
		await tick();
		expect(
			document.querySelector('.toast'),
			"the previous session's activation reported success to whoever is signed in now"
		).toBeNull();
	});

	it('a timer armed by the previous session does not clear the NEW toast', async () => {
		// THE DEFERRED CLASS, driven rather than reasoned about. The timer is
		// armed under identity A and fires 3s later; by then the page may belong
		// to identity B and be showing B's own toast. Clearing it then is a
		// commit by a session that has ended.
		vi.useFakeTimers();
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		call.resolve({});
		await vi.advanceTimersByTimeAsync(0);
		await tick();
		expect(
			document.querySelector('.toast')?.textContent,
			'no toast was shown, so the timer under test was never armed'
		).toContain('Activated');

		// Identity moves; the page paints a toast belonging to the NEW session.
		flipIdentity();
		const stale = document.querySelector('.toast');
		expect(stale, 'precondition: a toast is on screen when the timer fires').not.toBeNull();

		await vi.advanceTimersByTimeAsync(3100);
		await tick();

		// The listener is NOT notified here, deliberately: this leg is about the
		// timer's own fence, not about the reload clearing up after it.
		expect(
			document.querySelector('.toast'),
			"a timer armed by the previous session cleared a toast belonging to whoever is signed " +
				'in now — a commit by a session that has ended'
		).not.toBeNull();
	});

	it('a LOAD in flight when the identity moves writes none of the previous data', async () => {
		// The load path had no driven leg at all (codex round 2 [P2]): the source
		// guard permits the fence to sit AFTER the assignments, and nothing held
		// a load open across an identity change to notice. The implementation
		// ordering was right; the instrument could not say so.
		await mountPage();
		const before = document.querySelectorAll('.activate-btn').length;
		expect(before, 'precondition: the first load rendered entries').toBeGreaterThan(0);

		// A second load, held open, whose response belongs to the previous
		// identity by the time it resolves.
		auth.notify();
		await tick();
		const reload = await waitFor(() => {
			if (libraryCalls.length < 2) throw new Error('no second load yet');
			return libraryCalls[1]!;
		});
		flipIdentity();
		reload({
			categories: [
				{ name: 'git', conventions: [CONVENTION, { ...CONVENTION, title: 'Second' }] },
			],
		});
		await tick();
		await tick();

		expect(
			document.body.textContent,
			"a load that resolved after the identity moved painted the previous session's library"
		).not.toContain('Second');
	});

	it('the FAILURE path arms a fenced timer too, and it clears', async () => {
		// Only the success timer was driven (codex round 2 [P2]). Two of the four
		// timers live in catch arms, so a dead branch carrying both
		// `identityHeld()` and `toast = null` satisfied the source guard and the
		// whole suite — source text vouching for a path nothing executes.
		vi.useFakeTimers();
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		call.reject(new Error('nope'));
		await vi.advanceTimersByTimeAsync(0);
		await tick();
		expect(
			document.querySelector('.toast')?.textContent,
			'the failure path showed no toast — its timer was never armed, so this leg measures nothing'
		).toContain('Failed to activate');

		await vi.advanceTimersByTimeAsync(3100);
		await tick();
		expect(
			document.querySelector('.toast'),
			"the failure path's timer never clears its toast"
		).toBeNull();
	});

	it('a FAILURE-path timer armed by the previous session does not clear the NEW toast', async () => {
		vi.useFakeTimers();
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		call.reject(new Error('nope'));
		await vi.advanceTimersByTimeAsync(0);
		await tick();
		expect(document.querySelector('.toast'), 'precondition: a toast is on screen').not.toBeNull();

		flipIdentity();
		await vi.advanceTimersByTimeAsync(3100);
		await tick();
		expect(
			document.querySelector('.toast'),
			'a failure-path timer armed by the previous session cleared a toast belonging to whoever ' +
				'is signed in now'
		).not.toBeNull();
	});

	it("the PLAYBOOK success path arms a fenced timer too, and it clears", async () => {
		vi.useFakeTimers();
		await mountPage();
		clickActivatePlaybook();
		await tick();
		const btn = document.querySelector('.activate-btn') as HTMLButtonElement | null;
		expect(btn, 'the playbook activate button did not render').not.toBeNull();
		btn!.click();
		const call = await waitFor(() => {
			if (playbookCalls.length === 0) throw new Error('no playbook activate call yet');
			return playbookCalls[0]!;
		});
		call.resolve({});
		await vi.advanceTimersByTimeAsync(0);
		await tick();
		expect(
			document.querySelector('.toast')?.textContent,
			'the playbook success path showed no toast — its timer was never armed'
		).toContain('Activated');

		await vi.advanceTimersByTimeAsync(3100);
		await tick();
		expect(
			document.querySelector('.toast'),
			"the playbook path's timer never clears its toast"
		).toBeNull();
	});

	it('a playbook activation IN FLIGHT when the identity moves commits nothing', async () => {
		// The late-response half of the playbook path (codex round 3 [P2]).
		// Round 2 gave the playbook handler a control and a click-after-the-change
		// leg, which between them prove the request STARTS and that a preflight
		// refusal works — and say nothing about the post-await fence. Removing
		// that fence left the whole suite passing.
		await mountPage();
		clickActivatePlaybook();
		await tick();
		const btn = document.querySelector('.activate-btn') as HTMLButtonElement | null;
		expect(btn, 'the playbook activate button did not render').not.toBeNull();
		btn!.click();
		const call = await waitFor(() => {
			if (playbookCalls.length === 0) throw new Error('no playbook activate call yet');
			return playbookCalls[0]!;
		});
		flipIdentity();
		call.resolve({});
		await tick();
		await tick();
		expect(
			document.querySelector('.toast'),
			"the previous session's playbook activation reported success to whoever is signed in now"
		).toBeNull();
	});

	it("the PLAYBOOK failure path arms a fenced timer too, and it clears", async () => {
		// The FOURTH timer, and the one round 2 claimed coverage of without
		// having it (codex round 3 [P2]). Two of the four live in catch arms;
		// round 2 drove the convention one and stopped.
		vi.useFakeTimers();
		await mountPage();
		clickActivatePlaybook();
		await tick();
		const btn = document.querySelector('.activate-btn') as HTMLButtonElement | null;
		expect(btn, 'the playbook activate button did not render').not.toBeNull();
		btn!.click();
		const call = await waitFor(() => {
			if (playbookCalls.length === 0) throw new Error('no playbook activate call yet');
			return playbookCalls[0]!;
		});
		call.reject(new Error('nope'));
		await vi.advanceTimersByTimeAsync(0);
		await tick();
		expect(
			document.querySelector('.toast')?.textContent,
			'the playbook failure path showed no toast — its timer was never armed'
		).toContain('Failed to activate');

		await vi.advanceTimersByTimeAsync(3100);
		await tick();
		expect(
			document.querySelector('.toast'),
			"the playbook failure path's timer never clears its toast"
		).toBeNull();
	});

	it('CONTROL: the timer DOES clear the toast when the identity is unchanged', async () => {
		// Pairs with the stale-timer leg above, which on its own is satisfied by
		// a timer that clears nothing at all (codex round 1 [P2]). Fixing a
		// stale-write by making the write never happen is the failure this
		// family already shipped once.
		vi.useFakeTimers();
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		call.resolve({});
		await vi.advanceTimersByTimeAsync(0);
		await tick();
		expect(document.querySelector('.toast'), 'no toast to clear').not.toBeNull();

		await vi.advanceTimersByTimeAsync(3100);
		await tick();
		expect(
			document.querySelector('.toast'),
			'the toast never clears under an unchanged identity — the fence removed the behaviour ' +
				'instead of conditioning it'
		).toBeNull();
	});

	// ── activatePlaybook, driven rather than asserted about ────────────────

	function clickActivatePlaybook(): void {
		const tab = [...document.querySelectorAll('button')].find(
			(b) => b.textContent?.trim().toLowerCase().includes('playbook')
		) as HTMLButtonElement | undefined;
		expect(tab, 'the playbooks tab did not render').toBeDefined();
		tab!.click();
	}

	it('CONTROL: activating a playbook issues its own write', async () => {
		await mountPage();
		clickActivatePlaybook();
		await tick();
		const btn = document.querySelector('.activate-btn') as HTMLButtonElement | null;
		expect(btn, 'the playbook activate button did not render').not.toBeNull();
		btn!.click();
		await waitFor(() => {
			expect(
				playbookCalls.length,
				'the playbook control issued no write — the leg below would measure nothing'
			).toBe(1);
		});
	});

	it('a playbook click AFTER an identity move never issues the activation', async () => {
		await mountPage();
		clickActivatePlaybook();
		await tick();
		flipIdentity();
		const btn = document.querySelector('.activate-btn') as HTMLButtonElement | null;
		expect(btn, 'the playbook activate button did not render').not.toBeNull();
		btn!.click();
		await tick();
		await tick();
		expect(
			playbookCalls.length,
			"a playbook chosen from the previous identity's library was activated into the workspace " +
				'on screen now'
		).toBe(0);
	});

	it('recovers after an identity change and activates under the NEW identity', async () => {
		// "Did it ever resume?" — the opposite of every leg above. A fence that
		// latches shut and never lifts is not a safe guard, it is an outage with
		// good intentions, and it is harder to notice than what it replaced.
		await mountPage();

		flipIdentity();
		auth.notify();
		await tick();

		const reload = await waitFor(() => {
			if (libraryCalls.length < 2) throw new Error('the identity change did not trigger a reload');
			return libraryCalls[1]!;
		});
		reload({ categories: [{ name: 'git', conventions: [CONVENTION] }] });

		await waitFor(() => {
			if (!document.querySelector('.activate-btn')) {
				throw new Error('the library never came back after the identity change');
			}
		});

		const before = activateCalls.length;
		clickActivate();
		await waitFor(() => {
			expect(
				activateCalls.length,
				'the library is rendered but activating commits nothing — the fence latched shut and ' +
					'never lifted'
			).toBe(before + 1);
		});
	});

	it('CONTROL: the identity-change reload clears the previous toast', async () => {
		// Pairs with the timer leg: that one proves the stale TIMER must not
		// clear the new toast; this proves the listener DOES clear the old one,
		// so "the toast survived" can never be read as the page doing nothing.
		await mountPage();
		clickActivate();
		const call = await waitFor(() => {
			if (activateCalls.length === 0) throw new Error('no activate call yet');
			return activateCalls[0]!;
		});
		call.resolve({});
		await waitFor(() => {
			if (!document.querySelector('.toast')) throw new Error('no toast yet');
		});
		flipIdentity();
		auth.notify();
		await tick();
		expect(
			document.querySelector('.toast'),
			"the previous session's toast is still on screen after the identity changed"
		).toBeNull();
	});
});
