// Runs in the jsdom vitest project (see vitest.config.ts). `setup-jsdom.ts`
// polyfills the native <dialog> top-layer methods Modal.svelte drives.
//
// WHAT THESE TESTS ARE FOR. The composer's textarea is not the deliverable —
// the presence line is (PLAN-2558's "Honesty" rationale). So the assertions
// below are about the three-way distinction the surface has to keep straight:
//
//     N > 0        → send, with the caveat that connected ≠ delivered
//     N == 0       → do NOT send; a push with nothing listening is lost, not queued
//     can't tell   → send anyway, and SAY it is a guess
//
// Two of those three are easy to get wrong in the same direction — flattening
// "can't tell" into "nobody is there" — and a test that only asserted the
// rendered copy would not notice, because both states render a warning box.
// So every gating test asserts the MECHANISM (did `api.items.push` get called?)
// and not just the button's `disabled` attribute: a disabled attribute is an
// end state that a second, unrelated bug could also produce (CONVE-12).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

const pushMock = vi.fn();
const sessionsListMock = vi.fn();
const toastMock = vi.fn();

vi.mock('$lib/api/client', async () => {
	// Keep the REAL PadApiError: the component branches on `instanceof`, and a
	// stand-in class would make the 503 test pass for the wrong reason.
	const actual = await vi.importActual<typeof import('$lib/api/client')>('$lib/api/client');
	return {
		...actual,
		api: {
			items: { push: (...args: unknown[]) => pushMock(...args) },
			sessions: { list: () => sessionsListMock() }
		}
	};
});

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (...args: unknown[]) => toastMock(...args) }
}));

const { PadApiError } = await import('$lib/api/client');
const PushToAgentDialog = (await import('./PushToAgentDialog.svelte')).default;

function baseProps(overrides: Record<string, unknown> = {}) {
	return {
		open: true,
		onclose: vi.fn(),
		wsSlug: 'docapp',
		itemSlug: 'fix-the-thing',
		itemRef: 'TASK-5',
		itemTitle: 'Fix the thing',
		...overrides
	};
}

/** Sessions payload with `n` live entries. */
function sessions(n: number) {
	return {
		count: n,
		sessions: Array.from({ length: n }, (_, i) => ({
			id: `session-id-${i}`,
			label: `docapp-${i}`,
			pid: 1000 + i,
			connected_at: new Date(Date.now() - 60_000).toISOString()
		}))
	};
}

function button(name: string): HTMLButtonElement {
	const match = Array.from(document.querySelectorAll('button')).find(
		(b) => b.textContent?.trim().toLowerCase() === name.toLowerCase()
	);
	if (!match) {
		throw new Error(
			`no button named "${name}"; present: ${Array.from(document.querySelectorAll('button'))
				.map((b) => JSON.stringify(b.textContent?.trim()))
				.join(', ')}`
		);
	}
	return match as HTMLButtonElement;
}

/** The dialog's rendered text with HTML whitespace collapsed — the same
 *  normalization a browser applies when laying it out, so an assertion reads
 *  the sentence the user sees rather than the source's indentation. */
function bodyText(): string {
	return (document.body.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function textarea(): HTMLTextAreaElement {
	const el = document.querySelector('textarea');
	if (!el) throw new Error('composer textarea not found');
	return el as HTMLTextAreaElement;
}

/** The session-target picker (PLAN-2558 S5), or null when the presence
 *  state has no live sessions to pick between (it isn't rendered at all). */
function targetPicker(): HTMLSelectElement | null {
	return document.querySelector('select');
}

/** Mount, then let the initial presence read settle. */
async function mountSettled(props: Record<string, unknown> = {}) {
	const result = render(PushToAgentDialog, { props: baseProps(props) });
	await tick();
	flushSync();
	// Two flushes: one for the fetch promise, one for the effect it re-triggers.
	await tick();
	await tick();
	flushSync();
	return result;
}

beforeEach(() => {
	pushMock.mockReset().mockResolvedValue({
		ref: 'TASK-5',
		workspace: 'docapp',
		pushed: true,
		message: 'ok',
		delivered_sessions: 1
	});
	sessionsListMock.mockReset().mockResolvedValue(sessions(1));
	toastMock.mockReset();
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
	document.body.innerHTML = '';
});

describe('PushToAgentDialog — presence honesty', () => {
	it('with a live session: enables Push and does not promise delivery', async () => {
		await mountSettled();

		const body = bodyText();
		expect(body).toContain('1 session connected');
		// The whole point. If this ever starts asserting delivery, the surface
		// has begun claiming something the server has no way to know.
		expect(body).toMatch(/can’t confirm delivery|cannot confirm delivery/i);
		expect(body).not.toMatch(/will be delivered/i);

		expect(button('Push').disabled).toBe(false);
	});

	it('with zero sessions: refuses to send, and the click really does nothing', async () => {
		sessionsListMock.mockResolvedValue(sessions(0));
		await mountSettled();

		expect(bodyText()).toContain('No agent session is connected');
		expect(button('Push').disabled).toBe(true);

		// The `disabled` assertion above is the load-bearing one — it is what
		// fails if `noListeners` ever drops out of `canSend`. The click below is
		// a belt-and-braces check that no other path reaches the endpoint; being
		// honest about it, a disabled <button> also refuses to dispatch, so this
		// line on its own would pass for that reason too.
		await fireEvent.click(button('Push'));
		await tick();
		expect(pushMock).not.toHaveBeenCalled();
	});

	it('offers the clipboard instead when nothing is listening', async () => {
		sessionsListMock.mockResolvedValue(sessions(0));
		const writeText = vi.fn().mockResolvedValue(undefined);
		Object.defineProperty(navigator, 'clipboard', {
			value: { writeText },
			configurable: true
		});

		await mountSettled();
		await fireEvent.click(button('Copy instead'));
		await tick();

		expect(writeText).toHaveBeenCalledWith('Take a look at TASK-5 — Fix the thing');
		expect(pushMock).not.toHaveBeenCalled();
	});

	it('when presence is UNAVAILABLE (503) it says so and still allows sending', async () => {
		sessionsListMock.mockRejectedValue(
			new PadApiError({ code: 'unavailable', message: 'Session presence is not available' })
		);
		await mountSettled();

		const body = bodyText();
		// Must NOT render as "nobody is listening" — that is the lie
		// handleListSessions returns a 503 (rather than an empty list) to avoid.
		expect(body).toContain('Can’t tell whether any agent session is connected');
		expect(body).not.toContain('No agent session is connected');

		// The counterfactual leg: this state must behave DIFFERENTLY from the
		// zero-sessions state above. If 'unknown' ever gets flattened into
		// 'known zero', this is what catches it — the copy test alone would not,
		// since both states render a warning box.
		expect(button('Push').disabled).toBe(false);
		await fireEvent.click(button('Push'));
		await tick();
		expect(pushMock).toHaveBeenCalledTimes(1);
	});

	it('treats a transient network failure as unknown, not as zero', async () => {
		sessionsListMock.mockRejectedValue(new Error('network down'));
		await mountSettled();

		expect(bodyText()).toContain('Could not reach the server to check');
		expect(button('Push').disabled).toBe(false);
	});

	it('re-reads presence while open, so a session that drops mid-compose disarms Push', async () => {
		vi.useFakeTimers();
		sessionsListMock.mockResolvedValue(sessions(1));
		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(0);
		flushSync();
		expect(button('Push').disabled).toBe(false);

		// The session goes away; the next poll must notice.
		sessionsListMock.mockResolvedValue(sessions(0));
		await vi.advanceTimersByTimeAsync(10_000);
		flushSync();

		expect(button('Push').disabled).toBe(true);
		expect(bodyText()).toContain('No agent session is connected');
	});
});

describe('PushToAgentDialog — message handling', () => {
	it('sends the COLLAPSED message, not the raw textarea value', async () => {
		await mountSettled();

		await fireEvent.input(textarea(), { target: { value: 'line one\n\n   line two  ' } });
		await tick();
		await fireEvent.click(button('Push'));
		await tick();

		// Whitespace-collapsed exactly as the server would collapse it — so what
		// the user sees in the counter is what actually goes on the wire.
		expect(pushMock).toHaveBeenCalledWith('docapp', 'fix-the-thing', 'line one line two');
	});

	it('blocks an over-length message client-side rather than letting it 400', async () => {
		await mountSettled();

		await fireEvent.input(textarea(), { target: { value: 'a'.repeat(4097) } });
		await tick();

		expect(button('Push').disabled).toBe(true);
		expect(bodyText()).toContain('4097 / 4096');
		await fireEvent.click(button('Push'));
		await tick();
		expect(pushMock).not.toHaveBeenCalled();
	});

	it('blocks a whitespace-only message, matching the server empty check', async () => {
		await mountSettled();

		await fireEvent.input(textarea(), { target: { value: '   \n\t  ' } });
		await tick();

		expect(button('Push').disabled).toBe(true);
		await fireEvent.click(button('Push'));
		await tick();
		expect(pushMock).not.toHaveBeenCalled();
	});

	it('warns that a multi-line message arrives as one line', async () => {
		await mountSettled();

		await fireEvent.input(textarea(), { target: { value: 'first\nsecond' } });
		await tick();
		expect(bodyText()).toContain('arrives as one line');
	});

	it('prefills from the item and closes with an unconfirmed-delivery toast on success', async () => {
		const onclose = vi.fn();
		await mountSettled({ onclose });

		expect(textarea().value).toBe('Take a look at TASK-5 — Fix the thing');

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(pushMock).toHaveBeenCalledTimes(1);
		expect(onclose).toHaveBeenCalled();
		const [msg] = toastMock.mock.calls[0] ?? [];
		expect(String(msg)).toMatch(/isn’t confirmed|is not confirmed/);
	});

	it('re-arms Push after a STRUCTURED refusal — the server answered, so nothing was published', async () => {
		pushMock.mockRejectedValueOnce(
			new PadApiError({ code: 'bad_request', message: 'message must not be empty' })
		);
		await mountSettled();

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		// A 400 means the handler refused BEFORE publishing, so a corrected
		// resend is safe and the button must come back.
		expect(button('Push').disabled).toBe(false);
		expect(bodyText()).toContain('message must not be empty');
	});

	it('does NOT re-arm Push after an UNSTRUCTURED failure — the push may already have landed', async () => {
		// A rejected fetch / non-JSON 502: the request went out and we never
		// learned its fate. The handler publishes before writing its response,
		// so re-arming would offer the user a duplicate delivery on an endpoint
		// with no idempotency key.
		pushMock.mockRejectedValue(new TypeError('Failed to fetch'));
		await mountSettled();

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(button('Push').disabled).toBe(true);
		expect(bodyText()).toContain('can’t tell whether this was sent');
		expect(pushMock).toHaveBeenCalledTimes(1);

		// And the state is genuinely latched, not merely rendered: a second
		// click cannot get through.
		await fireEvent.click(button('Push'));
		await tick();
		expect(pushMock).toHaveBeenCalledTimes(1);
	});

	it('ignores a stale presence response that resolves after a newer one', async () => {
		vi.useFakeTimers();
		// First poll stalls; the second answers "zero" and lands first.
		let releaseFirst: (v: unknown) => void = () => {};
		sessionsListMock.mockImplementationOnce(
			() => new Promise((resolve) => (releaseFirst = resolve))
		);
		sessionsListMock.mockResolvedValue(sessions(0));

		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(10_000); // second poll fires and resolves zero
		flushSync();
		expect(button('Push').disabled).toBe(true);

		// Now the STALE first poll finally answers with one session. Applying it
		// would re-enable Push against a session list already known to be empty.
		releaseFirst(sessions(1));
		await vi.advanceTimersByTimeAsync(0);
		flushSync();

		expect(button('Push').disabled).toBe(true);
		expect(bodyText()).toContain('No agent session is connected');
	});

	it('stops waiting on a presence read that never settles, rather than leaving Push dead', async () => {
		vi.useFakeTimers();
		sessionsListMock.mockImplementation(() => new Promise(() => {})); // never settles
		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(0);
		flushSync();

		// While checking, Push is held — that part is deliberate.
		expect(button('Push').disabled).toBe(true);

		await vi.advanceTimersByTimeAsync(5_000);
		flushSync();

		// …but not forever, and not silently: it degrades to the honest
		// "can't tell" state, which allows sending.
		expect(bodyText()).toContain('Can’t tell whether any agent session is connected');
		expect(button('Push').disabled).toBe(false);
	});

	it('latches outcome-unknown on an UNRECOGNISED structured error, not just an unstructured one', async () => {
		// A gateway/proxy that returns a JSON envelope becomes a PadApiError
		// just like a handler refusal does — but it can be produced AFTER the
		// handler published. Treating "structured" as "definitely not
		// published" would re-arm Push on exactly this case, so the rule is a
		// whitelist of known pre-publish codes and everything else is ambiguous.
		pushMock.mockRejectedValue(
			new PadApiError({ code: 'bad_gateway', message: 'upstream exploded' })
		);
		await mountSettled();

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(button('Push').disabled).toBe(true);
		expect(bodyText()).toContain('can’t tell whether this was sent');
	});

	it('does not close a NEW item’s composer when a destroyed instance’s send resolves', async () => {
		// The dialog is {#key itemSlug}-remounted, so item B gets a fresh
		// instance with its own generation counter — A's in-flight send would
		// still see its own `gen` unchanged and call the SHARED parent onclose,
		// closing B's composer. Only per-instance liveness distinguishes them.
		let releasePush: (v: unknown) => void = () => {};
		pushMock.mockImplementationOnce(() => new Promise((resolve) => (releasePush = resolve)));

		const onclose = vi.fn();
		const { unmount } = render(PushToAgentDialog, { props: baseProps({ onclose }) });
		await tick();
		await tick();
		flushSync();

		await fireEvent.click(button('Push'));
		await tick();

		// The parent switches items: this instance is destroyed.
		unmount();
		await tick();

		// A's push now completes.
		releasePush({ ref: 'TASK-5', workspace: 'docapp', pushed: true, message: 'ok' });
		await tick();
		await tick();

		// The toast still fires — the push really happened — but the dead
		// instance must not reach for the shared close callback.
		expect(toastMock).toHaveBeenCalled();
		expect(onclose).not.toHaveBeenCalled();
	});

	it('expires a known count that stops refreshing rather than holding it as fact', async () => {
		vi.useFakeTimers();
		// First read succeeds; every later poll hangs forever.
		sessionsListMock.mockResolvedValueOnce(sessions(1));
		sessionsListMock.mockImplementation(() => new Promise(() => {}));

		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(0);
		flushSync();
		expect(bodyText()).toContain('1 session connected');
		expect(button('Push').disabled).toBe(false);

		// Three poll intervals with no answer takes us past the server's own
		// ~30s presence staleness bound. Past that, "1 session connected" is a
		// claim nothing supports.
		await vi.advanceTimersByTimeAsync(40_000);
		flushSync();

		expect(bodyText()).not.toContain('1 session connected');
		expect(bodyText()).toContain('Can’t tell whether any agent session is connected');
		// Still sendable — "can't tell" never blocks, only "known zero" does.
		expect(button('Push').disabled).toBe(false);
	});

	it('re-arms Push on a middleware refusal (CSRF), which is strictly pre-publish', async () => {
		pushMock.mockRejectedValueOnce(
			new PadApiError({ code: 'csrf_error', message: 'CSRF token mismatch' })
		);
		await mountSettled();

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(button('Push').disabled).toBe(false);
		expect(bodyText()).toContain('CSRF token mismatch');
		expect(bodyText()).not.toContain('can’t tell whether this was sent');
	});

	it('does not let a pre-expiry poll restore the count it just declared too old', async () => {
		vi.useFakeTimers();
		// First read answers "one". The poll issued at t=10s hangs long enough
		// to outlive the expiry, then finally answers — with the state as it was
		// at t=10s.
		let releaseStalled: (v: unknown) => void = () => {};
		sessionsListMock.mockResolvedValueOnce(sessions(1));
		sessionsListMock.mockImplementationOnce(
			() => new Promise((resolve) => (releaseStalled = resolve))
		);
		sessionsListMock.mockImplementation(() => new Promise(() => {}));

		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(0);
		flushSync();
		expect(bodyText()).toContain('1 session connected');

		await vi.advanceTimersByTimeAsync(40_000);
		flushSync();
		expect(bodyText()).toContain('Can’t tell whether any agent session is connected');

		// The stalled pre-expiry poll finally lands. Its data is older than the
		// expiry we just applied, so it must be dropped rather than reinstating
		// "1 session connected".
		releaseStalled(sessions(1));
		await vi.advanceTimersByTimeAsync(0);
		flushSync();

		expect(bodyText()).not.toContain('1 session connected');
		expect(bodyText()).toContain('Can’t tell whether any agent session is connected');
	});

	it('surfaces a failed push in the dialog and does NOT retry it', async () => {
		pushMock.mockRejectedValue(
			new PadApiError({ code: 'bad_request', message: 'message must not be empty' })
		);
		const onclose = vi.fn();
		await mountSettled({ onclose });

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(bodyText()).toContain('message must not be empty');
		// Stays open so the user can fix it, and — the endpoint having no
		// idempotency key — exactly one dispatch happened.
		expect(onclose).not.toHaveBeenCalled();
		expect(pushMock).toHaveBeenCalledTimes(1);
	});
});

describe('PushToAgentDialog — session targeting (PLAN-2558 S5, TASK-2588)', () => {
	it('renders a broadcast default plus one option per connected session', async () => {
		sessionsListMock.mockResolvedValue(sessions(2));
		await mountSettled();

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker with 2 live sessions');
		const options = Array.from(picker.options).map((o) => ({ value: o.value, text: o.textContent }));
		expect(options[0]).toEqual({ value: '', text: expect.stringContaining('All connected sessions (2)') });
		expect(options[1]).toEqual({ value: 'session-id-0', text: 'docapp-0 (pid 1000)' });
		expect(options[2]).toEqual({ value: 'session-id-1', text: 'docapp-1 (pid 1001)' });
	});

	it('does not render a picker with zero sessions — nothing to pick between', async () => {
		sessionsListMock.mockResolvedValue(sessions(0));
		await mountSettled();
		expect(targetPicker()).toBeNull();
	});

	it('defaults to broadcast: an untouched send keeps the exact pre-S5 3-argument call', async () => {
		sessionsListMock.mockResolvedValue(sessions(2));
		await mountSettled();

		await fireEvent.click(button('Push'));
		await tick();

		// No 4th argument at all — not even an explicit `undefined` — so this
		// stays byte-identical to every pre-S5 broadcast call site
		// (QuickActionsMenu's S4 dispatch included).
		expect(pushMock).toHaveBeenCalledWith(
			'docapp',
			'fix-the-thing',
			'Take a look at TASK-5 — Fix the thing'
		);
	});

	it('selecting a session passes its id as the 4th push argument', async () => {
		sessionsListMock.mockResolvedValue(sessions(2));
		await mountSettled();

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker');
		await fireEvent.change(picker, { target: { value: 'session-id-1' } });
		await fireEvent.click(button('Push'));
		await tick();

		expect(pushMock).toHaveBeenCalledWith(
			'docapp',
			'fix-the-thing',
			'Take a look at TASK-5 — Fix the thing',
			'session-id-1'
		);
	});

	it('a targeted miss (delivered_sessions=0) toasts, keeps the dialog open, drops back to broadcast, and re-reads presence — because zero delivery means nothing to duplicate by resending', async () => {
		sessionsListMock.mockResolvedValue(sessions(2));
		pushMock.mockResolvedValueOnce({
			ref: 'TASK-5',
			workspace: 'docapp',
			pushed: true,
			message: 'ok',
			delivered_sessions: 0
		});
		const onclose = vi.fn();
		await mountSettled({ onclose });

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker');
		await fireEvent.change(picker, { target: { value: 'session-id-0' } });
		const presenceReadsBeforeSend = sessionsListMock.mock.calls.length;

		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(toastMock).toHaveBeenCalledWith('that session is gone — refresh the list', 'error');
		// Still open — unlike every OTHER successful-response outcome, a
		// definitive zero-delivery result is safe to leave re-armed rather
		// than closing, since nothing was actually sent to duplicate.
		expect(onclose).not.toHaveBeenCalled();
		// The stale selection drops back to broadcast, and presence is
		// re-read so the picker reflects who's actually still connected.
		expect(targetPicker()?.value).toBe('');
		expect(sessionsListMock.mock.calls.length).toBeGreaterThan(presenceReadsBeforeSend);
	});

	it('a successful targeted push (delivered_sessions > 0) closes normally, same as broadcast', async () => {
		sessionsListMock.mockResolvedValue(sessions(2));
		pushMock.mockResolvedValueOnce({
			ref: 'TASK-5',
			workspace: 'docapp',
			pushed: true,
			message: 'ok',
			delivered_sessions: 1
		});
		const onclose = vi.fn();
		await mountSettled({ onclose });

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker');
		await fireEvent.change(picker, { target: { value: 'session-id-0' } });
		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(onclose).toHaveBeenCalled();
		expect(toastMock).toHaveBeenCalledTimes(1);
		const [msg] = toastMock.mock.calls[0] ?? [];
		expect(String(msg)).toMatch(/isn’t confirmed|is not confirmed/);
	});

	it('a refresh that removes the selected session drops the picker back to broadcast — the next send carries no target_session_id', async () => {
		vi.useFakeTimers();
		sessionsListMock.mockResolvedValue(sessions(2));
		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(0);
		flushSync();

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker');
		await fireEvent.change(picker, { target: { value: 'session-id-0' } });
		expect(targetPicker()?.value).toBe('session-id-0');

		// The next poll's list no longer has session-id-0 — the OTHER
		// session is still there, so this is a picker-visible refresh, not
		// a drop to zero (which is already covered by presence-honesty's
		// "session drops mid-compose" test).
		sessionsListMock.mockResolvedValue({
			count: 1,
			sessions: [
				{
					id: 'session-id-1',
					label: 'docapp-1',
					pid: 1001,
					connected_at: new Date(Date.now() - 60_000).toISOString()
				}
			]
		});
		await vi.advanceTimersByTimeAsync(10_000);
		flushSync();

		// A <select> whose selected <option> vanished falls back to
		// DISPLAYING the remaining default while the bound value can stay
		// stale — this asserts the bound value itself, not just the
		// rendered option list.
		expect(targetPicker()?.value).toBe('');

		await fireEvent.click(button('Push'));
		await tick();

		// The pre-S5 3-argument shape — no target_session_id at all, not
		// even the now-dead 'session-id-0'.
		expect(pushMock).toHaveBeenCalledWith(
			'docapp',
			'fix-the-thing',
			'Take a look at TASK-5 — Fix the thing'
		);
	});

	it('a refresh that keeps the selected session live does not clobber the selection', async () => {
		vi.useFakeTimers();
		sessionsListMock.mockResolvedValue(sessions(2));
		render(PushToAgentDialog, { props: baseProps() });
		await vi.advanceTimersByTimeAsync(0);
		flushSync();

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker');
		await fireEvent.change(picker, { target: { value: 'session-id-0' } });

		// Same two sessions again — session-id-0 is still present.
		sessionsListMock.mockResolvedValue(sessions(2));
		await vi.advanceTimersByTimeAsync(10_000);
		flushSync();

		expect(targetPicker()?.value).toBe('session-id-0');

		await fireEvent.click(button('Push'));
		await tick();

		expect(pushMock).toHaveBeenCalledWith(
			'docapp',
			'fix-the-thing',
			'Take a look at TASK-5 — Fix the thing',
			'session-id-0'
		);
	});

	it('a targeted send whose response omits delivered_sessions entirely (mixed-version server) shows an info toast and closes — never the miss-flow', async () => {
		sessionsListMock.mockResolvedValue(sessions(2));
		// A pre-S5 server's response shape: no delivered_sessions key at
		// all, not even 0. A server that doesn't know about targeting
		// still unconditionally publishes every push it accepts (the
		// pre-S5 contract), so this is NOT the same as a same-version 0.
		pushMock.mockResolvedValueOnce({
			ref: 'TASK-5',
			workspace: 'docapp',
			pushed: true,
			message: 'ok'
		});
		const onclose = vi.fn();
		await mountSettled({ onclose });

		const picker = targetPicker();
		if (!picker) throw new Error('expected a target picker');
		await fireEvent.change(picker, { target: { value: 'session-id-0' } });
		await fireEvent.click(button('Push'));
		await tick();
		await tick();

		expect(toastMock).toHaveBeenCalledWith(
			'server didn’t confirm targeting — sent as broadcast',
			'info'
		);
		// Never the miss-toast — an absent field is UNKNOWN, not a
		// confirmed 0, and must never be treated as one.
		expect(toastMock).not.toHaveBeenCalledWith(
			'that session is gone — refresh the list',
			expect.anything()
		);
		// Dismissed like a normal success, not re-armed like a miss —
		// "no auto-resend enablement" (dispatcher round 2).
		expect(onclose).toHaveBeenCalled();
	});
});
