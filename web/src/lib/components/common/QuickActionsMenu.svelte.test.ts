import { describe, it, expect, vi, beforeEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { tick } from 'svelte';
import type { Collection, Item, QuickAction } from '$lib/types';

// BUG-2265 Pattern C: QuickActionsMenu's save must recover from a competing
// RENAME (404 not_found) — not just a 409 — by resolving the collection by its
// STABLE id and retrying against the fresh slug. This mounts the REAL component
// with a mocked api and drives the create-action save through a not_found on
// the FIRST update, asserting the retry targets the renamed slug.

const updateMock = vi.fn();
const listMock = vi.fn();
const sessionsListMock = vi.fn();
const pushMock = vi.fn();

// Stand-in for the real PadApiError. `$lib/push/dispatch` classifies failures
// with `err instanceof PadApiError`, and it imports the class from THIS mocked
// module — so the class the component sees and the class a test throws must be
// the same object, which is only true if both come from here.
class MockPadApiError extends Error {
	code: string;
	constructor(init: { code: string; message: string }) {
		super(init.message);
		this.code = init.code;
	}
}

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...args: unknown[]) => updateMock(...args),
			list: (...args: unknown[]) => listMock(...args),
		},
		sessions: {
			list: (...args: unknown[]) => sessionsListMock(...args),
		},
		items: {
			push: (...args: unknown[]) => pushMock(...args),
		},
	},
	PadApiError: MockPadApiError,
	// Real-ish classifier so the component's branch fires for 404/409.
	isConflictOrNotFound: (err: unknown) =>
		err instanceof Error &&
		((err as { code?: string }).code === 'not_found' ||
			(err as { code?: string }).code === 'update_conflict'),
}));

const copyMock = vi.fn();
vi.mock('$lib/utils/clipboard', () => ({
	copyToClipboard: (...args: unknown[]) => copyMock(...args),
}));

const toastShow = vi.fn();
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (...a: unknown[]) => toastShow(...a) },
}));

vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: { isMobile: false },
}));

// Stub the sub-components rendered inside the create form so the test doesn't
// depend on their internals.
vi.mock('$lib/components/common/BottomSheet.svelte', () => ({ default: noopComponent() }));
vi.mock('$lib/components/common/EmojiPickerButton.svelte', () => ({ default: noopComponent() }));

function noopComponent() {
	// A minimal Svelte-5-compatible mountable component.
	return function () {};
}

const { default: QuickActionsMenu } = await import('./QuickActionsMenu.svelte');

function makeCollection(id: string, slug: string): Collection {
	return {
		id,
		workspace_id: 'ws-1',
		name: 'Tasks',
		slug,
		icon: '',
		description: '',
		schema: '{"fields":[]}',
		settings: '{"quick_actions":[]}',
		sort_order: 0,
		is_default: false,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'TASK',
	} as Collection;
}

function notFound() {
	const e = new Error('gone') as Error & { code: string };
	e.code = 'not_found';
	return e;
}

describe('QuickActionsMenu save retry (BUG-2265 Pattern C)', () => {
	beforeEach(() => {
		updateMock.mockReset();
		listMock.mockReset();
		toastShow.mockReset();
	});

	it('recovers from a 404 (renamed slug) by resolving the collection by id', async () => {
		// First update (against the captured "old" slug) hits a rename → 404.
		// The retry lists collections, finds the SAME id under the new slug, and
		// updates against that new slug.
		updateMock
			.mockRejectedValueOnce(notFound())
			.mockResolvedValueOnce(makeCollection('c1', 'renamed-tasks'));
		listMock.mockResolvedValueOnce([makeCollection('c1', 'renamed-tasks')]);

		const host = document.createElement('div');
		document.body.appendChild(host);
		const onupdated = vi.fn();
		const component = mount(QuickActionsMenu, {
			target: host,
			props: {
				actions: [],
				collection: makeCollection('c1', 'tasks'),
				scope: 'item',
				wsSlug: 'ws-1',
				canEdit: true,
				oncollectionupdated: onupdated,
			},
		});
		flushSync();

		// Open the menu, then the inline create form.
		(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
		flushSync();
		const openFormBtn = [...document.querySelectorAll('button')].find((b) =>
			b.textContent?.includes('New quick action')
		) as HTMLButtonElement;
		openFormBtn.click();
		flushSync();

		// Fill label + prompt.
		const label = document.querySelector('.qa-label-input') as HTMLInputElement;
		const prompt = document.querySelector('.qa-prompt-input') as HTMLTextAreaElement;
		label.value = 'Ship';
		label.dispatchEvent(new Event('input', { bubbles: true }));
		prompt.value = '/pad ship';
		prompt.dispatchEvent(new Event('input', { bubbles: true }));
		flushSync();

		// Click Save.
		const saveBtn = [...document.querySelectorAll('button')].find(
			(b) => b.textContent?.trim() === 'Save'
		) as HTMLButtonElement;
		saveBtn.click();

		// Let the async save + retry settle.
		await vi.waitFor(() => expect(updateMock).toHaveBeenCalledTimes(2));
		await tick();

		// First update used the OLD slug; the retry used the NEW (renamed) slug.
		expect(updateMock.mock.calls[0][1]).toBe('tasks');
		expect(updateMock.mock.calls[1][1]).toBe('renamed-tasks');
		expect(listMock).toHaveBeenCalledTimes(1);
		// The retry succeeded → success toast + callback.
		expect(onupdated).toHaveBeenCalledTimes(1);

		unmount(component);
		host.remove();
	});
});

// ── PLAN-2558 S4: quick actions push, clipboard is the fallback ─────────────
//
// The behavior under test is a ROUTING decision plus what the user is told
// about it. Both halves matter: a fallback that silently reports "Copied to
// clipboard" on a surface the user believes pushes is the failure this slice
// exists to prevent, and so is a push that claims a delivery nobody can
// confirm. Each case therefore asserts BOTH which call was made and which was
// not — an assertion on the push alone would stay green if the component also
// clobbered the clipboard every time.

const PUSH_ACTION: QuickAction = {
	label: 'Ship it',
	prompt: 'Ship {ref}',
	scope: 'item',
};

function makeItem(): Item {
	return {
		id: 'i1',
		workspace_id: 'ws-1',
		collection_id: 'c1',
		slug: 'ship-the-thing',
		title: 'Ship the thing',
		content: '',
		fields: '{"status":"open"}',
		item_number: 14,
		collection_prefix: 'TASK',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

interface MountOpts {
	scope?: 'item' | 'collection';
	item?: Item | null;
	actions?: QuickAction[];
}

function mountMenu(opts: MountOpts = {}) {
	const host = document.createElement('div');
	document.body.appendChild(host);
	const component = mount(QuickActionsMenu, {
		target: host,
		props: {
			actions: opts.actions ?? [PUSH_ACTION],
			item: opts.item === undefined ? makeItem() : opts.item,
			collection: makeCollection('c1', 'tasks'),
			scope: opts.scope ?? 'item',
			wsSlug: 'ws-1',
			canEdit: false,
		},
	});
	flushSync();
	return { host, component };
}

/** Open the menu and let the presence read (if any) settle. */
async function openMenu(host: HTMLElement) {
	(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
	flushSync();
	await tick();
	await tick();
	flushSync();
}

function actionRow(host: HTMLElement, label: string): HTMLButtonElement {
	const row = [...document.querySelectorAll('button')].find(
		(b) => b.textContent?.trim() === label
	);
	if (!row) throw new Error(`no action row labelled ${label}`);
	return row as HTMLButtonElement;
}

describe('QuickActionsMenu push dispatch (PLAN-2558 S4)', () => {
	beforeEach(() => {
		updateMock.mockReset();
		listMock.mockReset();
		toastShow.mockReset();
		sessionsListMock.mockReset();
		pushMock.mockReset();
		copyMock.mockReset();
		copyMock.mockResolvedValue(true);
	});

	it('reports the SERVER\'s delivered_sessions, not the stale preflight count', async () => {
		// BUG-2698 codex round 5. The preflight poll saw three sessions; by
		// the time the push landed the server matched one. The toast used to
		// announce the preflight number because the response was discarded.
		sessionsListMock.mockResolvedValue({
			sessions: [
				{ id: 's1', armed: true },
				{ id: 's2', armed: true },
				{ id: 's3', armed: true }
			],
			count: 3
		});
		pushMock.mockResolvedValue({ pushed: true, delivered_sessions: 1 });

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));

		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());
		// The wrong behaviour's fingerprint, asserted directly: the stale 3.
		expect(toastShow.mock.calls[0][0]).not.toContain('3 agent sessions');
		expect(toastShow.mock.calls[0][0]).toContain('Pushed to your agent session');

		unmount(component);
		host.remove();
	});

	it('claims no number when the server could not count the delivery', async () => {
		// delivered_sessions: null — published, but the presence registry was
		// unreadable. Falling back to the preflight count here would assert a
		// figure the server just said it could not produce.
		sessionsListMock.mockResolvedValue({
			sessions: [
				{ id: 's1', armed: true },
				{ id: 's2', armed: true }
			],
			count: 2
		});
		pushMock.mockResolvedValue({ pushed: true, delivered_sessions: null });

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));

		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());
		const message = toastShow.mock.calls[0][0];
		expect(message).toContain('Pushed');
		expect(message).toContain('delivery isn’t confirmed');
		// Any digit would mean a count was invented — the stale 2, or a 0.
		expect(message).not.toMatch(/\d/);

		unmount(component);
		host.remove();
	});

	it('falls back to the preflight count when the server omits the field', async () => {
		// A server predating session targeting sends no delivered_sessions at
		// all. That is the one case where the preflight count is still the
		// best answer available — the control that keeps the two tests above
		// from passing for an implementation that simply never names a number.
		sessionsListMock.mockResolvedValue({
			sessions: [
				{ id: 's1', armed: true },
				{ id: 's2', armed: true }
			],
			count: 2
		});
		pushMock.mockResolvedValue({ pushed: true });

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));

		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());
		expect(toastShow.mock.calls[0][0]).toContain('2 agent sessions');

		unmount(component);
		host.remove();
	});

	it('pushes the resolved prompt when a session is connected, and leaves the clipboard alone', async () => {
		sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }], count: 1 });
		pushMock.mockResolvedValue({ pushed: true });

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));

		// Template resolved (TASK-14, not the literal {ref}) and addressed by
		// the item's slug against the workspace it was mounted for.
		expect(pushMock.mock.calls[0]).toEqual(['ws-1', 'ship-the-thing', 'Ship TASK-14']);
		// The clipboard is NOT a belt-and-braces second delivery: clobbering it
		// on the happy path would take the user's clipboard for nothing.
		expect(copyMock).not.toHaveBeenCalled();
		expect(toastShow.mock.calls[0][0]).toContain('Pushed to your agent session');
		expect(toastShow.mock.calls[0][0]).toContain('delivery isn’t confirmed');

		unmount(component);
		host.remove();
	});

	it('copies with the ruled wording when nothing is listening, and never pushes', async () => {
		sessionsListMock.mockResolvedValue({ sessions: [], count: 0 });

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		expect(pushMock).not.toHaveBeenCalled();
		expect(copyMock).toHaveBeenCalledWith('Ship TASK-14');
		expect(toastShow.mock.calls[0][0]).toBe(
			'No agent session accepting pushes — copied to clipboard instead'
		);

		unmount(component);
		host.remove();
	});

	it('copies when sessions are connected but NONE are armed (PLAN-2613 S4), never pushing', async () => {
		// Two sessions connected, neither accepting pushes. A push would be
		// dropped server-side, so the action must copy — the same routing as
		// zero sessions, decided on the ACCEPTING count not the connected one.
		sessionsListMock.mockResolvedValue({
			sessions: [
				{ id: 's1', armed: false },
				{ id: 's2', armed: false }
			],
			count: 2
		});

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		expect(pushMock).not.toHaveBeenCalled();
		expect(copyMock).toHaveBeenCalledWith('Ship TASK-14');
		expect(toastShow.mock.calls[0][0]).toBe(
			'No agent session accepting pushes — copied to clipboard instead'
		);

		unmount(component);
		host.remove();
	});

	it('copies rather than pushing when presence cannot be read', async () => {
		// A failed read is NOT zero sessions — but here it routes the same way,
		// because the surface has no dialog in which to state the uncertainty
		// and the lossless branch is the one to take blind.
		sessionsListMock.mockRejectedValue(new Error('network down'));

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		expect(pushMock).not.toHaveBeenCalled();
		expect(copyMock).toHaveBeenCalledWith('Ship TASK-14');
		expect(toastShow.mock.calls[0][0]).toContain('Couldn’t check for agent sessions');

		unmount(component);
		host.remove();
	});

	it('issues the clipboard fallback synchronously inside the click', async () => {
		// The load-bearing timing property, and the reason presence is read when
		// the MENU OPENS rather than on the click. Both clipboard APIs want the
		// user gesture that is live during the handler and gone after a network
		// round-trip, so the copy must be dispatched before ANY await.
		//
		// Counterfactual: move the presence read into handleAction (await it
		// before deciding) and this assertion fails while every other test in
		// this block still passes — the copy still happens, just a microtask
		// too late to be inside the gesture.
		sessionsListMock.mockResolvedValue({ sessions: [], count: 0 });

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		// No await, no tick: the call must already have been made.
		expect(copyMock).toHaveBeenCalledWith('Ship TASK-14');

		unmount(component);
		host.remove();
	});

	it('copies the RAW prompt but pushes the collapsed one', async () => {
		// Two different constraints. `Notification.Summary` is a single-line wire
		// contract, so a push is collapsed; the clipboard has no such bound, so a
		// paste should look like what the author wrote.
		const multiline: QuickAction = {
			label: 'Review',
			prompt: 'Review {ref}\n\nCheck the tests too.',
			scope: 'item',
		};

		sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }], count: 1 });
		pushMock.mockResolvedValue({ pushed: true });
		const pushed = mountMenu({ actions: [multiline] });
		await openMenu(pushed.host);
		actionRow(pushed.host, 'Review').click();
		await vi.waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));
		expect(pushMock.mock.calls[0][2]).toBe('Review TASK-14 Check the tests too.');
		unmount(pushed.component);
		pushed.host.remove();

		sessionsListMock.mockResolvedValue({ sessions: [], count: 0 });
		const copied = mountMenu({ actions: [multiline] });
		await openMenu(copied.host);
		actionRow(copied.host, 'Review').click();
		await vi.waitFor(() => expect(copyMock).toHaveBeenCalled());
		expect(copyMock.mock.calls[0][0]).toBe('Review TASK-14\n\nCheck the tests too.');
		unmount(copied.component);
		copied.host.remove();
	});

	it('never reads presence or pushes for a collection-scope action', async () => {
		// The endpoint is POST .../items/{slug}/push. A collection-scope action
		// has no item to address, so it keeps the pre-S4 behavior exactly — and
		// must not spend a presence read finding that out.
		const collectionAction: QuickAction = {
			label: 'Triage',
			prompt: 'Triage {collection}',
			scope: 'collection',
		};
		const { host, component } = mountMenu({
			scope: 'collection',
			item: null,
			actions: [collectionAction],
		});
		await openMenu(host);

		expect(sessionsListMock).not.toHaveBeenCalled();
		actionRow(host, 'Triage').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		expect(pushMock).not.toHaveBeenCalled();
		expect(copyMock).toHaveBeenCalledWith('Triage Tasks');
		expect(toastShow.mock.calls[0][0]).toBe('Copied to clipboard');

		unmount(component);
		host.remove();
	});

	it('offers a copy — but does not take one — when the server refuses before publishing', async () => {
		// A recognised pre-publish refusal means nothing went out, so handing the
		// text over cannot deliver it twice. It is OFFERED rather than done
		// because the original gesture is spent by now; the toast button is a
		// fresh one, which is what makes the clipboard write work.
		sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }], count: 1 });
		pushMock.mockRejectedValue(
			new MockPadApiError({ code: 'unavailable', message: 'Push is not available' })
		);

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		const [message, tone, , , action] = toastShow.mock.calls[0];
		expect(tone).toBe('error');
		expect(message).toContain('Push is not available');
		expect(action?.label).toBe('Copy instead');
		// Offered, not taken.
		expect(copyMock).not.toHaveBeenCalled();

		action.onAction();
		await vi.waitFor(() => expect(copyMock).toHaveBeenCalledWith('Ship TASK-14'));
		// Taking the offer reports its own outcome, plainly — the user asked for
		// the copy, so there is no absent push to explain.
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalledTimes(2));
		expect(toastShow.mock.calls[1][0]).toBe('Copied to clipboard');

		unmount(component);
		host.remove();
	});

	it('does not swallow a failure in the offered copy', async () => {
		// Taking the offer dismisses the toast that carried it, so a discarded
		// result would leave a failed copy completely silent — and this is the
		// one path where that silence means the instruction was neither sent NOR
		// copied, the worst state the surface can be in.
		sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }], count: 1 });
		pushMock.mockRejectedValue(
			new MockPadApiError({ code: 'rate_limited', message: 'Too many requests' })
		);
		copyMock.mockResolvedValue(false);

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		const action = toastShow.mock.calls[0][4];
		action.onAction();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalledTimes(2));
		expect(toastShow.mock.calls[1][0]).toBe('Failed to copy to clipboard');
		expect(toastShow.mock.calls[1][1]).toBe('error');

		unmount(component);
		host.remove();
	});

	it('offers nothing when the push outcome is unknown', async () => {
		// The handler publishes BEFORE it writes its response, so an
		// unrecognised failure leaves the instruction possibly delivered. A
		// "Copy instead" button here would invite exactly the duplicate the
		// message warns about — the endpoint has no idempotency key.
		sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }], count: 1 });
		pushMock.mockRejectedValue(new TypeError('Failed to fetch'));

		const { host, component } = mountMenu();
		await openMenu(host);
		actionRow(host, 'Ship it').click();
		await vi.waitFor(() => expect(toastShow).toHaveBeenCalled());

		const [message, tone, , , action] = toastShow.mock.calls[0];
		expect(tone).toBe('info');
		expect(message).toContain('didn’t say whether the push went through');
		expect(action).toBeUndefined();
		expect(copyMock).not.toHaveBeenCalled();

		unmount(component);
		host.remove();
	});

	it('tells the user which way the next click will go, before they click', async () => {
		// The S3 principle applied to a surface with no dialog: the routing is
		// visible in the menu itself, not only in the toast afterwards.
		sessionsListMock.mockResolvedValue({ sessions: [], count: 0 });
		const { host, component } = mountMenu();
		await openMenu(host);
		expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
			'No agent session connected — actions copy to your clipboard'
		);
		unmount(component);
		host.remove();

		sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }, { id: 's2', armed: true }], count: 2 });
		const live = mountMenu();
		await openMenu(live.host);
		expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
			'Pushes to 2 accepting sessions'
		);
		unmount(live.component);
		live.host.remove();
	});

	it('shows the accepting-of-connected split when some sessions are unarmed (PLAN-2613 S4)', async () => {
		// 3 connected, 1 accepting — the honest split D3 wants, not a bare "1".
		sessionsListMock.mockResolvedValue({
			sessions: [
				{ id: 's1', armed: true },
				{ id: 's2', armed: false },
				{ id: 's3', armed: false }
			],
			count: 3
		});
		const { host, component } = mountMenu();
		await openMenu(host);
		expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
			'Pushes to 1 accepting session (of 3 connected)'
		);
		unmount(component);
		host.remove();

		// Connected but none accepting: the tagline names both and points at
		// the enable path rather than reading as "nobody there".
		sessionsListMock.mockResolvedValue({
			sessions: [
				{ id: 's1', armed: false },
				{ id: 's2', armed: false }
			],
			count: 2
		});
		const none = mountMenu();
		await openMenu(none.host);
		expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
			'2 connected, 0 accepting pushes — actions copy; run /pad:connect to enable'
		);
		unmount(none.component);
		none.host.remove();
	});
});

describe('QuickActionsMenu presence staleness (codex round 1)', () => {
	beforeEach(() => {
		toastShow.mockReset();
		sessionsListMock.mockReset();
		pushMock.mockReset();
		copyMock.mockReset();
		copyMock.mockResolvedValue(true);
	});

	it('stops trusting a count whose refreshes have stopped landing', async () => {
		// A FAILED poll already degrades to 'unknown'. A poll that HANGS does
		// not — it just never writes, so without an expiry the menu goes on
		// offering a push against a session that may be long gone, which is the
		// one direction that loses the user's instruction.
		vi.useFakeTimers();
		try {
			sessionsListMock.mockResolvedValueOnce({ sessions: [{ id: 's1', armed: true }], count: 1 });
			// Every poll after the first hangs forever.
			sessionsListMock.mockReturnValue(new Promise(() => {}));

			const { host, component } = mountMenu();
			(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
			flushSync();
			await vi.advanceTimersByTimeAsync(0);
			flushSync();

			// The first read landed: the menu is armed to push.
			expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
				'Pushes to 1 accepting session'
			);

			// Four poll intervals later — nothing has refreshed it. (The TAGLINE
			// can lag the expiry by up to one interval, since only a tick
			// rewrites it; the routing decision does not — see the next test.)
			await vi.advanceTimersByTimeAsync(41_000);
			flushSync();
			expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
				'Can’t tell if an agent is connected — actions copy to your clipboard'
			);

			actionRow(host, 'Ship it').click();
			await vi.advanceTimersByTimeAsync(0);
			expect(pushMock).not.toHaveBeenCalled();
			expect(copyMock).toHaveBeenCalledWith('Ship TASK-14');
			expect(toastShow.mock.calls[0][0]).toContain('Couldn’t check for agent sessions');

			unmount(component);
			host.remove();
		} finally {
			vi.useRealTimers();
		}
	});

	it('refuses a stale count at click time even if the expiry timer never ran', async () => {
		// A timer is a request, not a guarantee: browsers throttle timers hard
		// in a backgrounded tab, so the expiry can fire long after it came due —
		// including after the user has come back and clicked. Modelled by moving
		// the CLOCK without running any timer, which is exactly what throttling
		// looks like from the component's point of view.
		vi.useFakeTimers();
		try {
			sessionsListMock.mockResolvedValueOnce({ sessions: [{ id: 's1', armed: true }], count: 1 });
			sessionsListMock.mockReturnValue(new Promise(() => {}));

			const { host, component } = mountMenu();
			(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
			flushSync();
			await vi.advanceTimersByTimeAsync(0);
			flushSync();

			// Clock jumps well past the bound; NO timer fires, so the display
			// still shows what the last successful read said.
			vi.setSystemTime(Date.now() + 5 * 60_000);
			flushSync();
			expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
				'Pushes to 1 accepting session'
			);

			// The decision must not inherit that staleness.
			actionRow(host, 'Ship it').click();
			await vi.advanceTimersByTimeAsync(0);
			expect(pushMock).not.toHaveBeenCalled();
			expect(copyMock).toHaveBeenCalledWith('Ship TASK-14');

			unmount(component);
			host.remove();
		} finally {
			vi.useRealTimers();
		}
	});

	it('flips the footer line the moment the answer comes due, not a poll later', async () => {
		// The line's whole job is to say what the next click will do, so it must
		// not go on advertising a push for up to a full poll interval after the
		// routing has already switched to the clipboard.
		vi.useFakeTimers();
		try {
			sessionsListMock.mockResolvedValueOnce({ sessions: [{ id: 's1', armed: true }], count: 1 });
			sessionsListMock.mockReturnValue(new Promise(() => {}));

			const { host, component } = mountMenu();
			(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
			flushSync();
			await vi.advanceTimersByTimeAsync(0);
			flushSync();

			// Just before the bound: still armed.
			await vi.advanceTimersByTimeAsync(29_000);
			flushSync();
			expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
				'Pushes to 1 accepting session'
			);

			// Just past it — and well before the next 10s poll tick at t=40s.
			await vi.advanceTimersByTimeAsync(1_500);
			flushSync();
			expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
				'Can’t tell if an agent is connected — actions copy to your clipboard'
			);

			unmount(component);
			host.remove();
		} finally {
			vi.useRealTimers();
		}
	});

	it('does not expire an answer that is still being refreshed', async () => {
		// The control leg: with polls landing, the same elapsed time must NOT
		// downgrade the answer — an expiry that fires on a healthy connection
		// would silently retire the whole feature.
		vi.useFakeTimers();
		try {
			sessionsListMock.mockResolvedValue({ sessions: [{ id: 's1', armed: true }], count: 1 });

			const { host, component } = mountMenu();
			(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
			flushSync();
			await vi.advanceTimersByTimeAsync(31_000);
			flushSync();

			expect(document.querySelector('.dropdown-tagline')?.textContent?.trim()).toBe(
				'Pushes to 1 accepting session'
			);
			actionRow(host, 'Ship it').click();
			await vi.advanceTimersByTimeAsync(0);
			expect(pushMock).toHaveBeenCalledTimes(1);
			expect(copyMock).not.toHaveBeenCalled();

			unmount(component);
			host.remove();
		} finally {
			vi.useRealTimers();
		}
	});
});
