import { describe, expect, it } from 'vitest';
import { createRawBase, isOwnBody } from './rawBase';

describe('raw editor base (BUG-3540)', () => {
	it('the first edit of a burst freezes the base; later edits do not move it', () => {
		const b = createRawBase();
		b.noteEdit(5, 'seed');
		b.noteEdit(9, 'something newer');
		expect(b.current).toEqual({ seq: 5, content: 'seed' });
	});

	it('a landed save moves the base to the row it wrote; reset clears it and the sent texts', () => {
		const b = createRawBase();
		b.noteEdit(5, 'seed');
		b.noteSent('mine 1');
		b.landed(6, 'mine 1');
		expect(b.current).toEqual({ seq: 6, content: 'mine 1' });
		b.reset();
		expect(b.current).toBeNull();
		expect(b.ownTexts()).toEqual([]);
	});

	it('own texts are the base and what this tab sent, newest kept, bounded', () => {
		const b = createRawBase();
		b.noteEdit(1, 'seed');
		for (let i = 0; i < 12; i++) b.noteSent(`t${i}`);
		const own = b.ownTexts();
		expect(own[0]).toBe('seed');
		expect(own).toContain('t11');
		expect(own).not.toContain('t0');
		expect(own.length).toBe(9);
	});
});

describe('isOwnBody (BUG-3540, exact only)', () => {
	it('matches a body this tab has exactly', () => {
		expect(isOwnBody('a\nb', ['x', 'a\nb'])).toBe(true);
	});

	it('refuses any difference, whitespace included', () => {
		expect(isOwnBody('a\nb ', ['a\nb'])).toBe(false);
		expect(isOwnBody('a\n\nb', ['a\nb'])).toBe(false);
		expect(isOwnBody('', ['a'])).toBe(false);
	});

	it("matches the editor's canonical form of an own text, and nothing looser", () => {
		const canon = (md: string) => md.replace(/^\* /gm, '- ');
		expect(isOwnBody('- item', ['* item'], canon)).toBe(true);
		expect(isOwnBody('- item ', ['* item'], canon)).toBe(false);
		expect(isOwnBody('- item', ['* item'], () => null)).toBe(false);
	});
});
