import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
	PUSH_MESSAGE_MAX_LEN,
	collapsePushMessage,
	defaultPushMessage,
	isPushMessageEmpty,
	isPushMessageTooLong,
	pushMessageLength
} from './message';

/**
 * The other half of the cross-language contract test described in
 * internal/server/push_message_collapse_test.go.
 *
 * The fixture is shared deliberately. The claim this module makes — "the
 * composer counts a message the way the server will" — is a claim about Go's
 * `unicode.IsSpace` versus JavaScript's `\s`, and those two disagree in BOTH
 * directions (U+0085 is whitespace to Go only, U+FEFF to JS only). Asserting
 * against a table written here would only pin what the author BELIEVED Go
 * does; the Go test pins the same table to what Go actually does, so a drift
 * on either side turns exactly one suite red.
 */
const fixture = JSON.parse(
	readFileSync(
		fileURLToPath(new URL('../../../../internal/server/testdata/push_message_cases.json', import.meta.url)),
		'utf8'
	)
) as { cases: { name: string; raw: string; collapsed: string; runes: number }[] };

describe('push message collapse — server contract fixture', () => {
	// Same false-green guard as the Go side: an empty (or truncated) fixture
	// would make every it() below vacuous while the suite reported green.
	it('loads the full shared fixture', () => {
		expect(fixture.cases.length).toBeGreaterThanOrEqual(20);
	});

	for (const tc of fixture.cases) {
		it(tc.name, () => {
			expect(collapsePushMessage(tc.raw)).toBe(tc.collapsed);
			expect(pushMessageLength(tc.raw)).toBe(tc.runes);
		});
	}

	// The two divergence cases are the reason this file exists, so assert the
	// mechanism directly rather than trusting that the table above still
	// contains them. Written as a counterfactual: these are precisely the
	// inputs on which a naive `\s`-based implementation would differ, so if
	// someone "simplifies" the whitespace class back to `\s`, these fail while
	// the ASCII cases keep passing.
	it('collapses U+0085 NEL, which JS \\s does not treat as whitespace', () => {
		expect('ab'.replace(/\s+/g, ' ')).toBe('ab'); // what \s would do: nothing
		expect(collapsePushMessage('ab')).toBe('a b'); // what Go does
	});

	it('preserves U+FEFF BOM, which JS \\s does treat as whitespace', () => {
		expect('a﻿b'.replace(/\s+/g, ' ')).toBe('a b'); // what \s would do: collapse
		expect(collapsePushMessage('a﻿b')).toBe('a﻿b'); // what Go does
	});
});

describe('push message bounds', () => {
	it('mirrors the server constant', () => {
		// Pinned on the Go side too (TestMaxPushMessageLenIsMirroredByTheWebComposer),
		// so a change to either without the other goes red.
		expect(PUSH_MESSAGE_MAX_LEN).toBe(4096);
	});

	it('treats whitespace-only as empty, matching the server 400', () => {
		expect(isPushMessageEmpty('')).toBe(true);
		expect(isPushMessageEmpty('   \n\t  ')).toBe(true);
		expect(isPushMessageEmpty(' 　')).toBe(true);
		expect(isPushMessageEmpty(' x ')).toBe(false);
	});

	it('accepts a message at exactly the bound', () => {
		const atLimit = 'a'.repeat(PUSH_MESSAGE_MAX_LEN);
		expect(pushMessageLength(atLimit)).toBe(PUSH_MESSAGE_MAX_LEN);
		expect(isPushMessageTooLong(atLimit)).toBe(false);
	});

	it('rejects one rune over the bound', () => {
		expect(isPushMessageTooLong('a'.repeat(PUSH_MESSAGE_MAX_LEN + 1))).toBe(true);
	});

	/**
	 * The regression this module exists to prevent, stated as its own case: a
	 * message whose RAW length is over the bound but whose collapsed length is
	 * under it. A composer counting the textarea value would refuse to send
	 * this; the server accepts it happily.
	 */
	it('counts the collapsed form, so whitespace padding does not eat the budget', () => {
		const padded = 'word' + '\n'.repeat(PUSH_MESSAGE_MAX_LEN + 100) + 'word';
		// The raw value is over the bound; only the collapse brings it under.
		expect(padded.length).toBeGreaterThan(PUSH_MESSAGE_MAX_LEN);
		expect(pushMessageLength(padded)).toBe(9); // "word word"
		expect(isPushMessageTooLong(padded)).toBe(false);
	});

	/**
	 * The mirror-image regression: astral characters. `String.length` counts
	 * UTF-16 units, so a message of emoji would look twice as long as the
	 * server measures it — a composer using `.length` would block at half the
	 * real budget.
	 */
	it('counts an astral character as one rune, not two UTF-16 units', () => {
		const emoji = '👍'.repeat(100);
		expect(emoji.length).toBe(200);
		expect(pushMessageLength(emoji)).toBe(100);
	});
});

describe('defaultPushMessage', () => {
	it('names the item and stays on one line', () => {
		const msg = defaultPushMessage('TASK-5', 'Fix the thing');
		expect(msg).toContain('TASK-5');
		expect(msg).toContain('Fix the thing');
		// Single-line: the channel collapses newlines, so a prefill that
		// contained one would misrepresent what gets sent.
		expect(collapsePushMessage(msg)).toBe(msg);
	});

	it('degrades to whichever half it has', () => {
		expect(defaultPushMessage('TASK-5', '')).toContain('TASK-5');
		expect(defaultPushMessage('', 'Fix the thing')).toContain('Fix the thing');
		expect(defaultPushMessage('', '')).toBe('');
	});
});
