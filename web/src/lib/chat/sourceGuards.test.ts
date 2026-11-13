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

	// -webkit-touch-callout: none suppresses only iOS's callout menu on the row.
	// It does not stop text selection (user-select is untouched); the long press opens our action menu instead.
	it('keeps the iOS touch callout off the message row', () => {
		const row = Object.entries(sources).find(([n]) => n.endsWith('/MessageBubble.svelte'))![1];
		expect(row).toMatch(/\.bubble\s*\{[^}]*-webkit-touch-callout:\s*none/);
	});

	// Bodies carry links now, and the long press declines to arm on a link, so a link keeps the
	// system's own long-press actions (open in a new tab, copy link).
	it('leaves the system link callout on links inside the message row', () => {
		const row = Object.entries(sources).find(([n]) => n.endsWith('/MessageBubble.svelte'))![1];
		expect(row).toMatch(/\.bubble\s+:global\(a\)\s*\{[^}]*-webkit-touch-callout:\s*default/);
	});

	// TASK-4515: chat controls are Button / IconButton, which carry their own CSS over app.css's
	// global button reset. A raw <button> is left only where the control is not button-shaped; each
	// one counted here is a known remainder, and the count only goes down.
	it('uses raw <button> only where the census allows', () => {
		const allowed: Record<string, number> = {
			'ChatInboxView.svelte': 2,
			'MessageBubble.svelte': 5,
			'PendingBubble.svelte': 2,
			'RosterRow.svelte': 1
		};
		const found: Record<string, number> = {};
		for (const [name, src] of Object.entries(sources)) {
			const count = (src.match(/<button[\s>]/g) ?? []).length;
			if (count) found[name.split('/').pop()!] = count;
		}
		expect(found).toEqual(allowed);
	});

	// --radius-md is not a token (app.css defines --radius-sm, --radius and --radius-lg).
	it('names no undefined radius token', () => {
		for (const [name, src] of Object.entries(sources)) expect(src, name).not.toMatch(/--radius-md/);
	});

	// The composer paints no slab of its own, and its text is 16px on a phone (no iOS focus zoom).
	it('keeps the composer transparent and 16px on the size axis', () => {
		const c = Object.entries(sources).find(([n]) => n.endsWith('/Composer.svelte'))![1];
		expect(c).not.toMatch(/background:\s*var\(--bg-primary\)/);
		const size = c.match(/@media \(max-width: 768px\), \(pointer: coarse\) \{([\s\S]*?)\n\t\}/);
		expect(size, 'size-axis media block').not.toBeNull();
		expect(size![1]).toMatch(/textarea\s*\{[^}]*font-size:\s*16px/);
	});

	// U32: no direction toggle and no accept-instructions control in the chat UI (flex-direction is CSS).
	it('has no direction or accept-instructions control', () => {
		for (const [name, src] of Object.entries(sources)) {
			expect(src, name).not.toMatch(/(?<!flex-)direction|accept[-_]instructions/i);
		}
	});
});
