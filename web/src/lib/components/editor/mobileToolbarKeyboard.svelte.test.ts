import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';

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

const keyboard = vi.hoisted(() => ({ visible: false }));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { get keyboardVisible() { return keyboard.visible; } },
}));

vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: { isMobile: true, isCoarsePointer: true, dragDisabled: true },
	MOBILE_BREAKPOINT: 768,
	MOBILE_MEDIA_QUERY: '(max-width: 768px)',
	COARSE_POINTER_QUERY: '(pointer: coarse)',
}));

const { default: Editor } = await import('./Editor.svelte');
const { page } = await import('$app/state');

class VisualViewportStub extends EventTarget {
	height = 775;
	offsetTop = 0;
}

let target: HTMLElement;
let instance: ReturnType<typeof mount> | undefined;
let tiptap: TiptapEditor;
let visualViewport: VisualViewportStub;

beforeEach(() => {
	vi.useFakeTimers();
	keyboard.visible = false;
	(page as { params: Record<string, string> }).params = { workspace: 'ws' };
	visualViewport = new VisualViewportStub();
	vi.stubGlobal('visualViewport', visualViewport);
	vi.stubGlobal('innerHeight', 775);
	target = document.body.appendChild(document.createElement('div'));
});

afterEach(async () => {
	if (instance) await unmount(instance);
	instance = undefined;
	target.remove();
	vi.unstubAllGlobals();
	vi.useRealTimers();
});

function showEditor(innerHeight: number, viewportHeight: number, keyboardVisible: boolean, focused = true) {
	vi.stubGlobal('innerHeight', innerHeight);
	visualViewport.height = viewportHeight;
	keyboard.visible = keyboardVisible;
	instance = mount(Editor, { target, props: {
		content: 'A paragraph.', itemId: 'item-keyboard', editable: true,
		onEditor: (editor: TiptapEditor) => { tiptap = editor; },
	} });
	flushSync();
	visualViewport.dispatchEvent(new Event('resize'));
	if (focused) {
		// jsdom has no caret geometry to scroll into view. Tiptap focuses on the next frame.
		tiptap.commands.focus(undefined, { scrollIntoView: false });
		vi.advanceTimersByTime(20);
	}
	flushSync();
	expect(tiptap.isFocused).toBe(focused);
	return document.querySelector<HTMLElement>('.mobile-toolbar');
}

describe('mobile toolbar keyboard visibility (BUG-4461)', () => {
	it('shows the toolbar and Attach file in a shrinking host', () => {
		const toolbar = showEditor(463, 463, true);
		expect(toolbar).toBeInTheDocument();
		expect(toolbar!.style.bottom).toBe('0px');
		expect(toolbar!.querySelector('button[aria-label="Attach file"]')).toBeInTheDocument();
	});

	it('hides the toolbar after keyboard dismissal while the editor stays focused', () => {
		expect(showEditor(775, 775, false)).not.toBeInTheDocument();
	});

	it('keeps the toolbar above an overlaying keyboard without the store signal', () => {
		const toolbar = showEditor(775, 463, false);
		expect(toolbar).toBeInTheDocument();
		expect(toolbar!.style.bottom).toBe('312px');
	});

	it('hides the toolbar when the editor is not focused', () => {
		expect(showEditor(463, 463, true, false)).not.toBeInTheDocument();
	});
});
