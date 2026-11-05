import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';
import type { MarkdownStorage } from 'tiptap-markdown';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

vi.mock('$lib/api/client', () => ({
	api: {
		server: { capabilities: () => new Promise(() => {}) },
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
			transform: vi.fn(),
			upload: vi.fn(),
		},
		items: { list: vi.fn(async () => []) },
	},
}));

const { default: BodyEditor } = await import('./Editor.svelte');
const { page } = await import('$app/state');

const { default: CommentEditor } = await import('../CommentEditor.svelte');
const CONTENT = 'A **fixed** paragraph.';
const HTML = '<p>A <strong>fixed</strong> paragraph.</p>';
let target: HTMLElement;
let instance: ReturnType<typeof mount>;
let tiptap: TiptapEditor;

beforeEach(() => {
	vi.useFakeTimers();
	(page as { params: Record<string, string> }).params = { workspace: 'ws' };
	target = document.body.appendChild(document.createElement('div'));
});
afterEach(() => {
	if (instance) unmount(instance);
	target.remove();
	vi.useRealTimers();
});
function body(ariaLabel?: string) {
	instance = mount(BodyEditor, { target, props: {
		content: CONTENT, itemId: 'item-a11y', ariaLabel,
		onEditor: (editor: TiptapEditor) => { tiptap = editor; },
	} });
	flushSync();
	return target.querySelector<HTMLElement>('.ProseMirror')!;
}
function comment(ariaLabel?: string, onSubmit = vi.fn()) {
	instance = mount(CommentEditor, { target, props: { content: CONTENT, wsSlug: 'ws', ariaLabel, onSubmit } });
	flushSync();
	return target.querySelector<HTMLElement>('.ProseMirror')!;
}
function name(surface: HTMLElement, label: string) {
	expect(surface).toHaveAccessibleName(label);
	expect(surface).toHaveAttribute('aria-multiline', 'true');
	expect(surface.getAttribute('role')?.split(/\s+/)).toEqual(['textbox']);
	expect(surface.getAttributeNames().filter(attr => attr === 'role')).toHaveLength(1);
}

describe('editor DOM accessibility attributes (BUG-4336)', () => {
	it('the installed Tiptap already supplies exactly one textbox role', () => {
		expect(body().getAttribute('role')?.split(/\s+/)).toEqual(['textbox']);
	});
	it.each([undefined, 'Description'])('names the real item editor (%s)', (ariaLabel) => {
		name(body(ariaLabel), ariaLabel ?? 'Item content');
	});
	it.each([undefined, 'Edit comment'])('names the real comment editor (%s)', (ariaLabel) => {
		name(comment(ariaLabel), ariaLabel ?? 'Comment');
	});
	it('preserves the item document HTML and markdown byte for byte', () => {
		body();
		expect(tiptap.getHTML()).toBe(HTML);
		expect((tiptap.storage as { markdown: MarkdownStorage }).markdown.getMarkdown()).toBe(CONTENT);
	});
	it('preserves comment HTML and submitted markdown byte for byte', () => {
		const submit = vi.fn();
		const surface = comment('Edit comment', submit);
		expect(surface.innerHTML).toBe(HTML);
		target.querySelector<HTMLButtonElement>('.ce-submit')!.click();
		flushSync();
		expect(submit).toHaveBeenCalledWith(CONTENT);
	});
});
