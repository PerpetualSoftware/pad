// TASK-4515: the avatar tint is a stable pick from the accent tokens.
import { describe, expect, it } from 'vitest';
import { AVATAR_TONES, avatarInitial, avatarTone } from './avatar';

describe('avatarTone', () => {
	it('is stable for a key', () => {
		expect(avatarTone('rook')).toBe(avatarTone('rook'));
	});
	it('is always a token from the palette', () => {
		for (const k of ['rook', 'wren', 'codex', 'deploy-bot-eu-west', 'a', 'Z', '日本']) {
			expect(AVATAR_TONES).toContain(avatarTone(k));
		}
		expect(AVATAR_TONES).toHaveLength(7);
		for (const t of AVATAR_TONES) expect(t).toMatch(/^--accent-[a-z]+$/);
	});
	it('spreads keys across more than one tone', () => {
		const tones = new Set(['rook', 'wren', 'codex', 'night-shift', 'deploy-bot', 'kite', 'luna'].map(avatarTone));
		expect(tones.size).toBeGreaterThan(1);
	});
	it('an empty key still has a tone', () => {
		expect(AVATAR_TONES).toContain(avatarTone(''));
	});
});

describe('avatarInitial', () => {
	it('is the first character, upper-cased, and ? for none', () => {
		expect(avatarInitial('rook')).toBe('R');
		expect(avatarInitial('')).toBe('?');
		expect(avatarInitial('  ')).toBe('?');
	});
});
