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

	// U30 (TASK-4315 PR 2a): a body is injected as HTML in exactly one place, MessageBody, and only
	// the output of renderChatMarkdown (which ends in the Chat sanitiser profile). A comment may name
	// the tag; a use has an expression after it.
	it('uses {@html} only in MessageBody, and only for the rendered body', () => {
		for (const [name, src] of Object.entries(sources)) {
			if (name.endsWith('/MessageBody.svelte')) continue;
			expect(src, name).not.toMatch(/\{@html\s+[^}\s]/);
		}
		const body = Object.entries(sources).find(([n]) => n.endsWith('/MessageBody.svelte'))![1];
		expect(body.match(/\{@html\s+[^}\s][^}]*\}/g)).toEqual(['{@html html}']);
		expect(body).toMatch(/let html = \$derived\(renderChatMarkdown\(/);
		expect(body).not.toMatch(/innerHTML|insertAdjacentHTML|outerHTML|createContextualFragment/);
	});

	// U32: no direction toggle and no accept-instructions control in the chat UI (flex-direction is CSS).
	it('has no direction or accept-instructions control', () => {
		for (const [name, src] of Object.entries(sources)) {
			expect(src, name).not.toMatch(/(?<!flex-)direction|accept[-_]instructions/i);
		}
	});
});
