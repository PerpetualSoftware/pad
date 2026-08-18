/**
 * Routing a resolved prompt to a connected agent session — or to the clipboard
 * when it can't go to one (PLAN-2558 S4).
 *
 * S3 gave the item view a composer that answers "is anything listening?" before
 * the click, in a dialog with room to state the answer. A quick action has no
 * such moment: it is one click that must decide for the user and then report
 * what it did. This module is that decision, split into pure pieces so the
 * wording — which is the actual deliverable, since the whole plan exists to
 * stop a button claiming a delivery it can't make — is unit-testable without a
 * browser.
 *
 * WHY THE UNCERTAIN CASE GOES TO THE CLIPBOARD, WHERE S3'S DIALOG SENDS.
 * PushToAgentDialog leaves Send ENABLED when presence can't be read, and that
 * is right there: the warning is on screen, and the user chooses with the
 * uncertainty in front of them. Here nobody is asked. So the tie is broken by
 * which wrong answer costs more:
 *
 *   - copy when we could have pushed → the user pastes once. Exactly today's
 *     behavior, nothing lost.
 *   - push when nothing is listening → the message is gone. There is no inbox,
 *     no ack, no replay a user can reach, and the toast has already claimed
 *     success. That is the precise failure PLAN-2558 exists to prevent.
 *
 * Asymmetric, so the uncertain case takes the lossless branch.
 */

import { PadApiError } from '$lib/api/client';
import {
	PUSH_MESSAGE_MAX_LEN,
	isPushMessageEmpty,
	isPushMessageTooLong
} from './message';

/**
 * Error codes `handlePushToItem` (and the middleware in front of it) can return
 * WITHOUT having published — the request provably never reached the bus, so a
 * corrected resend cannot duplicate anything.
 *
 * A whitelist, not `err instanceof PadApiError`, and the difference is
 * load-bearing (PR #1099 codex round 2): the API client turns ANY JSON error
 * envelope into a PadApiError, including one a proxy or gateway invented AFTER
 * the handler had already published. Treating "structured" as "definitely not
 * published" would offer a duplicate on exactly that case. So the rule is
 * inverted — enumerate what is known safe, and treat every unrecognised failure
 * as ambiguous. The cost of the wrong answer is asymmetric: an unnecessary "we
 * can't tell" makes the user check, a wrong re-arm delivers twice.
 *
 * Lives here rather than in the dialog because S4 is the second consumer, and
 * two copies of a security-shaped whitelist drift.
 */
export const PUSH_PRE_PUBLISH_ERROR_CODES: ReadonlySet<string> = new Set([
	'bad_request', // empty / whitespace-only / over-length / undecodable body
	'unauthorized', // no resolved user
	'not_found', // item or workspace doesn't resolve
	'forbidden',
	'permission_denied', // workspace-access middleware
	'unavailable', // the bus isn't wired — nothing to publish TO
	'rate_limited', // the client's own 429 shape; the handler never ran
	'plan_limit_exceeded',
	// Middleware, so strictly before the handler: nothing can have been
	// published by the time either of these is written.
	'csrf_error',
	'email_not_verified'
]);

/** True when `err` is a failure the push endpoint provably wrote BEFORE
 *  publishing, so the message did not go out and re-offering it is safe. */
export function isPrePublishRefusal(err: unknown): boolean {
	const code = err instanceof PadApiError ? err.code : '';
	return code !== '' && PUSH_PRE_PUBLISH_ERROR_CODES.has(code);
}

/**
 * What a surface knows about who is listening.
 *
 * There is deliberately no `count: 0` inside `unknown` — a failed presence read
 * is not zero sessions, and collapsing the two is the exact lie
 * `handleListSessions` returns 503 rather than an empty list to avoid.
 */
export type PushPresence =
	| { state: 'known'; count: number }
	| { state: 'unknown' };

/** Why a prompt went to the clipboard instead of an agent session. */
export type ClipboardReason =
	/** The surface has no item to address — the push endpoint is item-scoped,
	 *  and a collection-scope quick action has nothing to point it at. */
	| 'not-addressable'
	/** The resolved prompt collapses to nothing; the server would 400. */
	| 'empty'
	/** Over the server's rune bound. The clipboard has no such bound, so the
	 *  text survives there — this is a downgrade, not a failure. */
	| 'too-long'
	/** The ruled case (PLAN-2558 S4): nothing is listening. */
	| 'no-sessions'
	/** Presence could not be read. See this module's header for why that goes
	 *  to the clipboard here and not in the dialog. */
	| 'presence-unknown'
	/** The user asked for the copy explicitly, by taking the offer on a
	 *  push-failure toast. Nothing to explain — they chose it. */
	| 'offered';

export type PushRoute = { via: 'push' } | { via: 'clipboard'; because: ClipboardReason };

/**
 * Decide where a resolved prompt goes. Pure: no I/O, no clock, no DOM.
 *
 * `presence` is null when the surface hasn't got an answer yet (the read is
 * still in flight). That is treated as `unknown` rather than given its own
 * branch — "haven't heard" and "couldn't hear" license exactly the same
 * conclusion, and a third state would only invite a fourth wording.
 */
export function routePrompt(
	prompt: string,
	itemSlug: string | null,
	presence: PushPresence | null
): PushRoute {
	if (!itemSlug) return { via: 'clipboard', because: 'not-addressable' };
	if (isPushMessageEmpty(prompt)) return { via: 'clipboard', because: 'empty' };
	if (isPushMessageTooLong(prompt)) return { via: 'clipboard', because: 'too-long' };
	if (!presence || presence.state === 'unknown') {
		return { via: 'clipboard', because: 'presence-unknown' };
	}
	if (presence.count === 0) return { via: 'clipboard', because: 'no-sessions' };
	return { via: 'push' };
}

/** What actually happened, once the route was taken. */
export type DispatchOutcome =
	| { kind: 'pushed'; count: number }
	| { kind: 'copied'; because: ClipboardReason }
	| { kind: 'copy-failed'; because: ClipboardReason }
	/** The server refused before publishing — nothing was sent, and offering
	 *  the text again is safe. */
	| { kind: 'push-refused'; detail: string }
	/** The request went out and we never learned its fate. The handler
	 *  publishes BEFORE it writes its response, so the message may well have
	 *  been delivered — the user must be told that, not told it failed. */
	| { kind: 'push-unconfirmed' };

export interface DispatchMessage {
	message: string;
	tone: 'success' | 'error' | 'info';
}

/**
 * The toast for an outcome.
 *
 * Two rules run through every string here:
 *  1. Never claim delivery. A push is published to a bus with no ack, so the
 *     honest past tense is "pushed", hedged — never "delivered" or "sent to
 *     your agent".
 *  2. Always name the fallback and why. "Copied to clipboard" alone, on a
 *     surface the user believes pushes, reads as the push having happened.
 */
export function describeDispatch(outcome: DispatchOutcome): DispatchMessage {
	switch (outcome.kind) {
		case 'pushed':
			return {
				message:
					outcome.count === 1
						? 'Pushed to your agent session — delivery isn’t confirmed'
						: `Pushed to ${outcome.count} agent sessions — delivery isn’t confirmed`,
				tone: 'success'
			};
		case 'copied':
			return { message: copiedMessage(outcome.because), tone: 'success' };
		case 'copy-failed':
			return { message: 'Failed to copy to clipboard', tone: 'error' };
		case 'push-refused':
			return {
				message: outcome.detail
					? `Couldn’t push: ${outcome.detail}`
					: 'Couldn’t push to your agent session',
				tone: 'error'
			};
		case 'push-unconfirmed':
			// Not an error and not a success: the server gave no clear answer, so
			// the only true thing to say is that we don't know. Copying for the
			// user here would be worse than useless — if the push DID land, a
			// paste delivers the same instruction twice, and the endpoint has no
			// idempotency key to absorb it. So the copy is offered, not taken.
			return {
				message:
					'The server didn’t say whether the push went through. Check your agent session before sending it again.',
				tone: 'info'
			};
	}
}

function copiedMessage(because: ClipboardReason): string {
	switch (because) {
		case 'no-sessions':
			// "accepting", not "connected": the caller counts only armed
			// sessions (PLAN-2613 S4), so this fires when nothing is ACCEPTING
			// pushes — which includes the case where sessions are connected but
			// none has opted in.
			return 'No agent session accepting pushes — copied to clipboard instead';
		case 'presence-unknown':
			return 'Couldn’t check for agent sessions — copied to clipboard instead';
		case 'too-long':
			return `Too long to push (over ${PUSH_MESSAGE_MAX_LEN} characters) — copied to clipboard instead`;
		case 'not-addressable':
		case 'empty':
		case 'offered':
			// Nothing surprising happened: these surfaces never push, so
			// explaining an absence the user never expected would be noise.
			return 'Copied to clipboard';
	}
}
