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

vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { keyboardVisible: true },
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
	height = 463;
	offsetTop = 0;
}

let target: HTMLElement;
let instance: ReturnType<typeof mount> | undefined;
let tiptap: TiptapEditor;

beforeEach(() => {
	vi.useFakeTimers();
	(page as { params: Record<string, string> }).params = { workspace: 'ws' };
	vi.stubGlobal('visualViewport', new VisualViewportStub());
	vi.stubGlobal('innerHeight', 463);
	target = document.body.appendChild(document.createElement('div'));
});

afterEach(async () => {
	if (instance) await unmount(instance);
	instance = undefined;
	target.remove();
	vi.unstubAllGlobals();
	vi.useRealTimers();
});

function showToolbar(): HTMLElement {
	instance = mount(Editor, { target, props: {
		content: 'A paragraph.', itemId: 'item-toolbar', editable: true,
		onEditor: (editor: TiptapEditor) => { tiptap = editor; },
	} });
	flushSync();
	tiptap.commands.focus(undefined, { scrollIntoView: false });
	vi.advanceTimersByTime(20);
	flushSync();
	const toolbar = document.querySelector<HTMLElement>('.mobile-toolbar');
	expect(toolbar).toBeInTheDocument();
	return toolbar!;
}

function setScroll(el: HTMLElement, dims: { clientWidth: number; scrollWidth: number; scrollLeft: number }) {
	Object.defineProperty(el, 'clientWidth', { configurable: true, value: dims.clientWidth });
	Object.defineProperty(el, 'scrollWidth', { configurable: true, value: dims.scrollWidth });
	Object.defineProperty(el, 'scrollLeft', { configurable: true, value: dims.scrollLeft });
	el.dispatchEvent(new Event('scroll'));
	flushSync();
}

describe('mobile toolbar polish (TASK-4521)', () => {
	it('draws every button as an icon, never a typed character', () => {
		const buttons = [...showToolbar().querySelectorAll<HTMLButtonElement>('button.mt-btn')];
		expect(buttons).toHaveLength(14);
		for (const button of buttons) {
			expect(button.getAttribute('aria-label'), button.outerHTML).toBeTruthy();
			expect(button.querySelector('svg'), button.getAttribute('aria-label')!).not.toBeNull();
			expect(button.textContent!.trim(), button.getAttribute('aria-label')!).toBe('');
		}
	});

	it('hides the icons from assistive technology, leaving the label', () => {
		for (const svg of showToolbar().querySelectorAll('button.mt-btn svg')) {
			expect(svg.getAttribute('aria-hidden')).toBe('true');
		}
	});

	it('shows no scroll hint when every button fits', () => {
		const toolbar = showToolbar();
		const scroller = toolbar.querySelector<HTMLElement>('.mt-scroll')!;
		setScroll(scroller, { clientWidth: 700, scrollWidth: 700, scrollLeft: 0 });
		expect(toolbar).not.toHaveClass('more-start');
		expect(toolbar).not.toHaveClass('more-end');
	});

	it('hints at the hidden buttons on whichever side has them', () => {
		const toolbar = showToolbar();
		const scroller = toolbar.querySelector<HTMLElement>('.mt-scroll')!;
		expect(scroller.contains(toolbar.querySelector('button[aria-label="Attach file"]'))).toBe(true);

		setScroll(scroller, { clientWidth: 400, scrollWidth: 700, scrollLeft: 0 });
		expect(toolbar).not.toHaveClass('more-start');
		expect(toolbar).toHaveClass('more-end');

		setScroll(scroller, { clientWidth: 400, scrollWidth: 700, scrollLeft: 150 });
		expect(toolbar).toHaveClass('more-start');
		expect(toolbar).toHaveClass('more-end');

		setScroll(scroller, { clientWidth: 400, scrollWidth: 700, scrollLeft: 300 });
		expect(toolbar).toHaveClass('more-start');
		expect(toolbar).not.toHaveClass('more-end');
	});
});
