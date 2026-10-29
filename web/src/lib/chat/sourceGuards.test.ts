// TASK-4044 3c: source guards for the chat UI (U30, U32).
import { describe, expect, it } from 'vitest';

const sources = {
	...import.meta.glob('../components/chat/*.svelte', { query: '?raw', import: 'default', eager: true }),
	...import.meta.glob('../../routes/messages/**/*.svelte', { query: '?raw', import: 'default', eager: true })
} as Record<string, string>;

describe('chat UI sources', () => {
	it('finds the sources it guards', () => {
		const names = Object.keys(sources);
		expect(names.some((n) => n.endsWith('MessageBubble.svelte'))).toBe(true);
		expect(names.some((n) => n.endsWith('[n]/+page.svelte'))).toBe(true);
	});

	// U30: a body is never injected as HTML. A comment may name the tag; a use has an expression after it.
	it('uses no {@html} tag', () => {
		for (const [name, src] of Object.entries(sources)) expect(src, name).not.toMatch(/\{@html\s+[^}\s]/);
	});

	// U32: no direction toggle and no accept-instructions control in the chat UI (flex-direction is CSS).
	it('has no direction or accept-instructions control', () => {
		for (const [name, src] of Object.entries(sources)) {
			expect(src, name).not.toMatch(/(?<!flex-)direction|accept[-_]instructions/i);
		}
	});
});
