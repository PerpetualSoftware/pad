// TASK-2235: DockedSheet focus. It claimed aria-modal while leaving the
// bottom nav live by design, and it neither took focus nor kept Tab out of
// the page its backdrop covers. It now drops aria-modal (it is not modal),
// takes focus on open, returns it on close, and cycles Tab through the sheet
// and the nav only. The cycle math is in paneFocus (nextTrapTargetAcross).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';
import DockedSheet from './DockedSheet.svelte';

// jsdom has no Web Animations API, which Svelte's fly/fade outros use. A
// stand-in that finishes at once (the ToastContainer test's): these tests are
// about focus, not motion.
if (!('animate' in Element.prototype)) {
	(Element.prototype as unknown as { animate: unknown }).animate = function () {
		const animation = {
			onfinish: null as null | (() => void),
			cancel() {},
			finish() {},
			currentTime: 0,
			addEventListener() {},
			finished: Promise.resolve()
		};
		queueMicrotask(() => animation.onfinish?.());
		return animation;
	};
}

const bodySnippet = createRawSnippet(() => ({
	render: () => `<div><button id="s1" type="button">One</button><button id="s2" type="button">Two</button></div>`
}));

function props(overrides: Record<string, unknown> = {}) {
	return { open: true, onclose: vi.fn(), label: 'Workspace', children: bodySnippet, ...overrides };
}

function panel(): HTMLElement {
	return document.querySelector('.ds-panel') as HTMLElement;
}

async function settle() {
	await tick();
	flushSync();
}

/** The bottom nav and the page behind the sheet, as the layout renders them. */
function mountChrome() {
	document.body.insertAdjacentHTML(
		'afterbegin',
		`<main><button id="page">Page</button></main>
		 <nav class="bottom-nav" aria-label="Primary"><button id="n1">Workspace</button><button id="n2">You</button></nav>`
	);
	return {
		page: document.getElementById('page') as HTMLElement,
		n1: document.getElementById('n1') as HTMLElement,
		n2: document.getElementById('n2') as HTMLElement
	};
}

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('DockedSheet focus (TASK-2235)', () => {
	it('is a labelled dialog that does not claim aria-modal, and takes focus on open', async () => {
		render(DockedSheet, { props: props() });
		await settle();
		const p = panel();
		expect(p.getAttribute('role')).toBe('dialog');
		expect(p.getAttribute('aria-label')).toBe('Workspace');
		expect(p.hasAttribute('aria-modal')).toBe(false);
		expect(document.activeElement).toBe(p);
	});

	it('cycles Tab through the sheet and then the nav, never the page', async () => {
		vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([
			{ width: 1, height: 1 } as DOMRect
		] as unknown as DOMRectList);
		const { n1, n2 } = mountChrome();
		render(DockedSheet, { props: props() });
		await settle();
		const s1 = document.getElementById('s1') as HTMLElement;
		const s2 = document.getElementById('s2') as HTMLElement;

		const tab = (shiftKey = false) => {
			const e = new KeyboardEvent('keydown', { key: 'Tab', shiftKey, cancelable: true });
			window.dispatchEvent(e);
			return e;
		};

		s2.focus();
		expect(tab().defaultPrevented).toBe(true);
		expect(document.activeElement).toBe(n1);
		n2.focus();
		tab();
		expect(document.activeElement).toBe(s1);
		tab(true);
		expect(document.activeElement).toBe(n2);
	});

	it('leaves Tab alone while closed', async () => {
		const { page } = mountChrome();
		render(DockedSheet, { props: props({ open: false }) });
		await settle();
		page.focus();
		const e = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true });
		window.dispatchEvent(e);
		expect(e.defaultPrevented).toBe(false);
		expect(document.activeElement).toBe(page);
	});

	it('returns focus to the trigger when it closes', async () => {
		const { n1 } = mountChrome();
		n1.focus();
		const view = render(DockedSheet, { props: props() });
		await settle();
		expect(document.activeElement).toBe(panel());
		await view.rerender(props({ open: false }));
		await settle();
		expect(document.activeElement).toBe(n1);
	});
});
