import { describe, it, expect } from 'vitest';
import { PadApiError } from '$lib/api/client';
import { PUSH_MESSAGE_MAX_LEN } from './message';
import {
	PUSH_PRE_PUBLISH_ERROR_CODES,
	describeDispatch,
	isPrePublishRefusal,
	routePrompt,
	type PushPresence
} from './dispatch';

const KNOWN = (count: number): PushPresence => ({ state: 'known', count });
const UNKNOWN: PushPresence = { state: 'unknown' };

describe('routePrompt', () => {
	it('pushes when an item is addressable and a session is connected', () => {
		expect(routePrompt('do the thing', 'task-1', KNOWN(1))).toEqual({ via: 'push' });
		expect(routePrompt('do the thing', 'task-1', KNOWN(4))).toEqual({ via: 'push' });
	});

	it('falls back to the clipboard when nothing is listening', () => {
		// The ruled case (PLAN-2558 S4). Not a hard error, not a queue.
		expect(routePrompt('do the thing', 'task-1', KNOWN(0))).toEqual({
			via: 'clipboard',
			because: 'no-sessions'
		});
	});

	it('treats an unreadable presence answer as clipboard, NOT as a push', () => {
		// The asymmetry this module exists for: copying when we could have
		// pushed costs a paste; pushing into nothing loses the instruction.
		expect(routePrompt('do the thing', 'task-1', UNKNOWN)).toEqual({
			via: 'clipboard',
			because: 'presence-unknown'
		});
	});

	it('treats "no answer yet" the same as "could not get an answer"', () => {
		expect(routePrompt('do the thing', 'task-1', null)).toEqual({
			via: 'clipboard',
			because: 'presence-unknown'
		});
	});

	it('cannot push without an item to address, however many sessions are live', () => {
		// Collection-scope quick actions: the endpoint is item-scoped.
		expect(routePrompt('do the thing', null, KNOWN(3))).toEqual({
			via: 'clipboard',
			because: 'not-addressable'
		});
	});

	it('refuses to push a message the server would reject', () => {
		// Both are 400s at the endpoint, so pushing them would trade a working
		// clipboard copy for an error. Checked BEFORE presence, so a live
		// session doesn't route an unpushable message into a failure.
		expect(routePrompt('   \n\t  ', 'task-1', KNOWN(2))).toEqual({
			via: 'clipboard',
			because: 'empty'
		});
		expect(routePrompt('x'.repeat(PUSH_MESSAGE_MAX_LEN + 1), 'task-1', KNOWN(2))).toEqual({
			via: 'clipboard',
			because: 'too-long'
		});
		// Exactly at the bound is pushable — the check is `>`, matching the
		// server's own comparison.
		expect(routePrompt('x'.repeat(PUSH_MESSAGE_MAX_LEN), 'task-1', KNOWN(2))).toEqual({
			via: 'push'
		});
	});

	it('measures length after collapse, like the server does', () => {
		// Raw length is over the bound; collapsed length is exactly at it. A
		// client counting the raw string would refuse a message the server
		// accepts — the failure `$lib/push/message` exists to prevent, asserted
		// here at the routing layer that consumes it.
		const raw = 'x'.repeat(PUSH_MESSAGE_MAX_LEN) + '\n'.repeat(200);
		expect(raw.length).toBeGreaterThan(PUSH_MESSAGE_MAX_LEN);
		expect(routePrompt(raw, 'task-1', KNOWN(1))).toEqual({ via: 'push' });
	});
});

describe('isPrePublishRefusal', () => {
	it('recognises the codes the handler and its middleware write before publishing', () => {
		for (const code of PUSH_PRE_PUBLISH_ERROR_CODES) {
			expect(isPrePublishRefusal(new PadApiError({ code, message: 'nope' }))).toBe(true);
		}
	});

	it('treats an unrecognised structured error as ambiguous', () => {
		// The load-bearing case: the API client turns ANY JSON error envelope
		// into a PadApiError, including one a gateway invented AFTER the
		// handler published. "Structured" is not proof of "never sent".
		expect(isPrePublishRefusal(new PadApiError({ code: 'upstream_error', message: 'bad gateway' }))).toBe(
			false
		);
		expect(isPrePublishRefusal(new PadApiError({ code: '', message: 'boom' }))).toBe(false);
	});

	it('treats a non-API failure as ambiguous', () => {
		expect(isPrePublishRefusal(new TypeError('Failed to fetch'))).toBe(false);
		expect(isPrePublishRefusal(undefined)).toBe(false);
	});
});

describe('describeDispatch', () => {
	it('never claims delivery on a successful push', () => {
		const one = describeDispatch({ kind: 'pushed', count: 1 });
		expect(one.tone).toBe('success');
		expect(one.message).toContain('delivery isn’t confirmed');
		expect(one.message).not.toMatch(/delivered|received/i);

		const many = describeDispatch({ kind: 'pushed', count: 3 });
		expect(many.message).toContain('3 agent sessions');
		expect(many.message).toContain('delivery isn’t confirmed');
	});

	it('names the fallback AND its reason, so a copy never reads as a push', () => {
		expect(describeDispatch({ kind: 'copied', because: 'no-sessions' }).message).toBe(
			'No agent session accepting pushes — copied to clipboard instead'
		);
		expect(describeDispatch({ kind: 'copied', because: 'presence-unknown' }).message).toContain(
			'Couldn’t check for agent sessions'
		);
		expect(describeDispatch({ kind: 'copied', because: 'too-long' }).message).toContain(
			String(PUSH_MESSAGE_MAX_LEN)
		);
	});

	it('says only "copied" where the user never expected a push', () => {
		// A collection-scope action has no push behavior to explain the absence
		// of, so explaining it would be noise.
		expect(describeDispatch({ kind: 'copied', because: 'not-addressable' }).message).toBe(
			'Copied to clipboard'
		);
		expect(describeDispatch({ kind: 'copied', because: 'empty' }).message).toBe(
			'Copied to clipboard'
		);
	});

	it('reports a refusal as a failure and carries the server’s reason', () => {
		const d = describeDispatch({ kind: 'push-refused', detail: 'Push is not available' });
		expect(d.tone).toBe('error');
		expect(d.message).toContain('Push is not available');
	});

	it('reports an unconfirmed push as neither success nor failure', () => {
		const d = describeDispatch({ kind: 'push-unconfirmed' });
		// Not 'error': the message may well have landed. Not 'success' either.
		expect(d.tone).toBe('info');
		expect(d.message).toMatch(/didn’t say|check your agent session/i);
		// It must NOT claim a copy happened — nothing was copied, precisely
		// because a paste could deliver the same instruction a second time.
		expect(d.message).not.toMatch(/clipboard|copied/i);
	});

	it('reports a failed copy as an error whatever sent it there', () => {
		for (const because of ['no-sessions', 'presence-unknown', 'not-addressable'] as const) {
			const d = describeDispatch({ kind: 'copy-failed', because });
			expect(d.tone).toBe('error');
			expect(d.message).toContain('Failed to copy');
		}
	});
});
