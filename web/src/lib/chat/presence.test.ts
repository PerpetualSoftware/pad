import { describe, it, expect } from 'vitest';
import { rollupPresence, normalizeAgentName, trimSpace } from './presence';
import type { LiveSession } from '$lib/types';

function sess(agent: string | undefined, state: LiveSession['state'], id = Math.random().toString()): LiveSession {
	return { agent, state, id, armed: state === 'online', connected_at: '2026-01-01T00:00:00Z' };
}

describe('rollupPresence', () => {
	it('online beats busy beats connected_not_accepting', () => {
		const out = rollupPresence([], [
			sess('a', 'connected_not_accepting'),
			sess('a', 'busy'),
			sess('a', 'online'),
			sess('b', 'connected_not_accepting'),
			sess('b', 'busy'),
			sess('c', 'connected_not_accepting')
		]);
		expect(out.get('a')).toBe('online');
		expect(out.get('b')).toBe('busy_uncertain');
		expect(out.get('c')).toBe('connected_not_accepting');
	});

	it('an agent with a DM but no session is offline', () => {
		expect(rollupPresence(['dm-only'], []).get('dm-only')).toBe('offline');
	});

	it('the roster is the union of DM keys and session agents', () => {
		const out = rollupPresence(['a'], [sess('b', 'online')]);
		expect([...out.keys()].sort()).toEqual(['a', 'b']);
	});

	it('skips sessions with no agent name', () => {
		const out = rollupPresence([], [sess(undefined, 'online'), sess('', 'online'), sess('  ', 'online')]);
		expect(out.size).toBe(0);
	});

	it('maps every agent to unknown when sessions are unavailable', () => {
		const out = rollupPresence(['a', 'b'], 'unavailable');
		expect(out.get('a')).toBe('unknown');
		expect(out.get('b')).toBe('unknown');
	});

	it('normalises session names like the server so they meet the DM key', () => {
		const out = rollupPresence(['claude'], [sess('  claude\u0007 ', 'online')]);
		expect(out.get('claude')).toBe('online');
		expect(out.size).toBe(1);
	});
});

describe('normalizeAgentName', () => {
	it('trims, strips control characters and caps at 64 runes', () => {
		expect(normalizeAgentName('\t a\nb ')).toBe('ab');
		expect(normalizeAgentName('é'.repeat(70))).toBe('é'.repeat(64));
		expect(normalizeAgentName('')).toBe('');
	});

	// Go's TrimSpace is the White_Space property, not JS trim() (BUG-4092). The
	// same inputs are pinned on the Go side in TestNormalizeAgentNameTrimParity.
	it('trims exactly what Go TrimSpace trims', () => {
		expect(normalizeAgentName('\uFEFFcodex\uFEFF')).toBe('\uFEFFcodex\uFEFF');
		expect(normalizeAgentName('  codex\u3000')).toBe('codex');
		expect(normalizeAgentName('\u0085 codex')).toBe('codex');
		expect(normalizeAgentName('\u200Bcodex')).toBe('\u200Bcodex');
		expect(normalizeAgentName('a'.repeat(63) + ' b')).toBe('a'.repeat(63));
	});
});

describe('trimSpace', () => {
	it('trims both ends with the White_Space set and keeps U+FEFF', () => {
		expect(trimSpace(' \t\u00a0\u3000x y\n ')).toBe('x y');
		expect(trimSpace('\uFEFFx\uFEFF')).toBe('\uFEFFx\uFEFF');
		expect(trimSpace('   ')).toBe('');
		expect(trimSpace('')).toBe('');
	});

	// r6 B2-M1: the regex form took ~2.8 s on this input; the composer runs it per keystroke.
	it('is linear on a long interior whitespace run', () => {
		const run = ' '.repeat(64000);
		const t0 = performance.now();
		expect(trimSpace('a' + run + 'b')).toBe('a' + run + 'b');
		expect(trimSpace(run + 'a' + run + 'b' + run)).toBe('a' + run + 'b');
		expect(trimSpace(run + 'a')).toBe('a');
		expect(trimSpace('a' + run)).toBe('a');
		expect(performance.now() - t0).toBeLessThan(200);
	});
});
