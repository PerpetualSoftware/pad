import { describe, it, expect, beforeEach } from 'vitest';
import {
	PANE_FOCUSABLE_SELECTOR,
	paneFocusables,
	nextTrapTarget,
	resolvePaneReturnTarget,
} from './paneFocus';

// jsdom has no layout engine, so `offsetParent` / `getClientRects` can't gate
// visibility here — the production default reads those, but every helper that
// needs them takes an injectable predicate. These tests pass an "everything
// visible" stub and assert the DOM-selection + cycle math directly.
const allVisible = () => true;

function mount(html: string): HTMLElement {
	document.body.innerHTML = `<div id="root">${html}</div>`;
	return document.getElementById('root') as HTMLElement;
}

beforeEach(() => {
	document.body.innerHTML = '';
});

describe('paneFocusables', () => {
	it('collects tabbable descendants in DOM order', () => {
		const el = mount(`
			<a href="/a">a</a>
			<button>b</button>
			<input />
			<div contenteditable="true">editor</div>
		`);
		const found = paneFocusables(el, allVisible);
		expect(found.map((n) => n.tagName.toLowerCase())).toEqual([
			'a',
			'button',
			'input',
			'div',
		]);
	});

	it('excludes tabindex="-1", disabled controls, and href-less anchors', () => {
		const el = mount(`
			<a>no-href</a>
			<button disabled>disabled</button>
			<input disabled />
			<div tabindex="-1">programmatic</div>
			<button tabindex="-1">skip</button>
			<a href="/keep">keep</a>
		`);
		const found = paneFocusables(el, allVisible);
		expect(found).toHaveLength(1);
		expect(found[0].getAttribute('href')).toBe('/keep');
	});

	it('honors the injected visibility predicate', () => {
		const el = mount(`<a href="/x" class="show">x</a><a href="/y" class="hide">y</a>`);
		const found = paneFocusables(el, (n) => n.classList.contains('show'));
		expect(found).toHaveLength(1);
		expect(found[0].getAttribute('href')).toBe('/x');
	});

	it('the selector includes positive/zero tabindex but not -1', () => {
		expect(PANE_FOCUSABLE_SELECTOR).toContain('[tabindex]:not([tabindex="-1"])');
	});
});

describe('nextTrapTarget', () => {
	function setup() {
		const container = mount(`<a href="/1">1</a><a href="/2">2</a><a href="/3">3</a>`);
		container.setAttribute('tabindex', '-1');
		const focusables = paneFocusables(container, allVisible);
		return { container, focusables, first: focusables[0], last: focusables[2] };
	}

	it('forward Tab off the last element wraps to the first', () => {
		const { container, focusables, first, last } = setup();
		expect(nextTrapTarget(focusables, last, false, container)).toBe(first);
	});

	it('Shift+Tab off the first element wraps to the last', () => {
		const { container, focusables, first, last } = setup();
		expect(nextTrapTarget(focusables, first, true, container)).toBe(last);
	});

	it('forward Tab mid-list returns null (let the browser move naturally)', () => {
		const { container, focusables } = setup();
		expect(nextTrapTarget(focusables, focusables[1], false, container)).toBeNull();
	});

	it('Shift+Tab mid-list returns null', () => {
		const { container, focusables } = setup();
		expect(nextTrapTarget(focusables, focusables[1], true, container)).toBeNull();
	});

	it('Shift+Tab from the region container itself wraps to the last', () => {
		const { container, focusables, last } = setup();
		expect(nextTrapTarget(focusables, container, true, container)).toBe(last);
	});

	it('forward Tab from the region container falls through to native move', () => {
		const { container, focusables } = setup();
		// Container contains itself → treated as "inside", not off-the-end.
		expect(nextTrapTarget(focusables, container, false, container)).toBeNull();
	});

	it('focus escaped outside the pane is pulled back to the edge', () => {
		const { container, focusables, first, last } = setup();
		const outside = document.createElement('button');
		document.body.appendChild(outside);
		expect(nextTrapTarget(focusables, outside, false, container)).toBe(first);
		expect(nextTrapTarget(focusables, outside, true, container)).toBe(last);
		expect(nextTrapTarget(focusables, null, false, container)).toBe(first);
	});

	it('an empty pane keeps focus on the container', () => {
		const container = mount('');
		expect(nextTrapTarget([], container, false, container)).toBe(container);
		expect(nextTrapTarget([], container, true, container)).toBe(container);
	});
});

describe('resolvePaneReturnTarget', () => {
	it('returns the focused list/board card anchor itself', () => {
		const root = mount(`
			<a href="/a" class="item-card">a</a>
			<a href="/b" class="item-card focused">b</a>
		`);
		const target = resolvePaneReturnTarget(root, null);
		expect(target?.getAttribute('href')).toBe('/b');
	});

	it('returns the title-link inside a focused table row', () => {
		const root = mount(`
			<div class="table-row" role="row"><a href="/a" class="title-link">a</a></div>
			<div class="table-row focused" role="row"><a href="/b" class="title-link">b</a></div>
		`);
		const target = resolvePaneReturnTarget(root, null);
		expect(target?.getAttribute('href')).toBe('/b');
	});

	it('falls back to the captured trigger when no focused row exists', () => {
		const root = mount(`<a href="/a" class="item-card">a</a>`);
		const captured = root.querySelector<HTMLElement>('a')!;
		expect(resolvePaneReturnTarget(root, captured)).toBe(captured);
	});

	it('prefers the focused row over the captured trigger (paged A→C returns to C)', () => {
		const root = mount(`
			<a href="/a" class="item-card">a</a>
			<a href="/c" class="item-card focused">c</a>
		`);
		const captured = root.querySelector<HTMLElement>('a[href="/a"]')!;
		const target = resolvePaneReturnTarget(root, captured);
		expect(target?.getAttribute('href')).toBe('/c');
	});

	it('ignores a captured trigger detached from the document', () => {
		const root = mount('');
		const detached = document.createElement('a');
		detached.href = '/gone';
		expect(detached.isConnected).toBe(false);
		expect(resolvePaneReturnTarget(root, detached)).toBeNull();
	});

	it('returns null when there is neither a focused row nor a live trigger', () => {
		const root = mount(`<a href="/a" class="item-card">a</a>`);
		expect(resolvePaneReturnTarget(root, null)).toBeNull();
	});
});
