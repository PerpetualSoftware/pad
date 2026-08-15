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
		message: 'ok'
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
