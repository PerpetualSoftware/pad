import { describe, it, expect, beforeEach } from 'vitest';
import {
	PANE_FOCUSABLE_SELECTOR,
	paneFocusables,
	nextTrapTarget,
	resolvePaneReturnTarget,
	inExemptSurface,
	handoffFocus,
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

describe('inExemptSurface', () => {
	it('does NOT treat the pane region itself as exempt (TASK-2131)', () => {
		// The mobile overlay carries role="dialog"; an in-pane control must not be
		// seen as an exempt foreign surface (that killed the mobile Tab trap and
		// confused the focus-follows classifier). Excluded via `:not(.item-pane)`.
		const root = mount(`
			<aside class="item-pane" role="dialog" aria-modal="true">
				<button id="in-pane">edit</button>
			</aside>
		`);
		expect(inExemptSurface(root.querySelector('#in-pane'))).toBe(false);
	});

	it('treats a nested dialog/menu opened FROM the pane as exempt', () => {
		// A real overlay (e.g. a BottomSheet or field-select menu) that portals in
		// while the pane is open still owns its own focus/keyboard.
		const root = mount(`
			<aside class="item-pane" role="dialog">
				<div role="menu"><button id="menu-item">move to…</button></div>
			</aside>
			<div role="dialog" id="sheet"><button id="sheet-btn">confirm</button></div>
		`);
		expect(inExemptSurface(root.querySelector('#menu-item'))).toBe(true);
		expect(inExemptSurface(root.querySelector('#sheet-btn'))).toBe(true);
	});

	it('treats a native <dialog> and standalone overlays as exempt; nothing else', () => {
		const root = mount(`
			<dialog open><button id="native">ok</button></dialog>
			<div role="listbox"><div id="opt">option</div></div>
			<div class="block-context-menu"><button id="ctx">turn into</button></div>
			<button id="plain">plain</button>
		`);
		expect(inExemptSurface(root.querySelector('#native'))).toBe(true);
		expect(inExemptSurface(root.querySelector('#opt'))).toBe(true);
		expect(inExemptSurface(root.querySelector('#ctx'))).toBe(true);
		expect(inExemptSurface(root.querySelector('#plain'))).toBe(false);
		expect(inExemptSurface(null)).toBe(false);
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

describe('handoffFocus (TASK-2456)', () => {
	// The modal-focus-retention helper: when a focused control leaves a surface
	// (removed or disabled), focus must not fall to <body> behind the inerted
	// background. Two shapes — reactive (no `departing`, repair after the fact)
	// and imperative (pass `departing`, hand off before removal/disable).

	it('CONTROL: without the handoff, removing a focused control strands focus on <body>', () => {
		// The defect the helper exists to fix, proven real in this environment so
		// the positive legs below are not vacuous: jsdom (like a real engine) drops
		// focus to <body> when the focused element is removed.
		const el = mount(`<button id="close">close</button><button id="next">next</button>`);
		const next = document.getElementById('next')!;
		next.focus();
		expect(document.activeElement).toBe(next);
		next.remove();
		expect(document.activeElement).toBe(document.body);
	});

	it('reactive shape: pulls focus off <body> back to the first tabbable fallback', () => {
		const el = mount(`<button id="close">close</button><button id="next">next</button>`);
		const next = document.getElementById('next')!;
		next.focus();
		next.remove();
		expect(document.activeElement).toBe(document.body);

		handoffFocus(el, null, allVisible);
		expect(document.activeElement).toBe(document.getElementById('close'));
	});

	it('reactive shape: leaves a live focus INSIDE the surface untouched', () => {
		// Only repairs a focus that has LEFT the surface — a focus resting on a
		// real control must not be yanked to the first tabbable.
		const el = mount(`<button id="close">close</button><button id="next">next</button>`);
		const close = document.getElementById('close')!;
		close.focus();
		handoffFocus(el, null, allVisible);
		expect(document.activeElement).toBe(close);
	});

	it('imperative shape: hands focus off a control about to be DISABLED, never back onto it', () => {
		// A real engine drops focus to <body> the instant a focused control is
		// disabled; jsdom keeps it there, so the imperative shape blurs explicitly
		// while the control still holds focus. This is the call TASK-2459/2460 make
		// before setting `disabled` on their retry / tap-to-load control.
		//
		// The departing control is ordered FIRST on purpose: it is `paneFocusables()
		// [0]`, so a fallback that did not EXCLUDE it would re-select the very
		// control about to be disabled — dropping focus to <body> on a real engine.
		const el = mount(`<button id="retry">retry</button><button id="close">close</button>`);
		const retry = document.getElementById('retry') as HTMLButtonElement;
		retry.focus();
		expect(document.activeElement).toBe(retry);

		handoffFocus(el, retry, allVisible);
		// The caller now DISABLES the control it just handed focus off — the exact
		// sequence TASK-2459/2460 run. On a real engine this is the step that would
		// drop focus to <body> had it still been on `retry`; because the handoff
		// moved focus to Close first, the disable is now harmless.
		retry.disabled = true;
		expect(document.activeElement).toBe(document.getElementById('close'));
		expect(document.activeElement).not.toBe(retry);
		expect(document.activeElement).not.toBe(document.body);
	});

	it('imperative shape: falls back to the container when the departing control is the ONLY tabbable', () => {
		// Mid-load a viewer's tap-to-load / retry control can be the only focusable
		// thing. Handing off before disabling it must NOT re-select it (→ <body> on
		// disable) — it lands on the container (`tabindex="-1"`), still inside the
		// surface.
		const el = mount(`<button id="tap">tap to load</button>`);
		el.tabIndex = -1;
		const tap = document.getElementById('tap') as HTMLButtonElement;
		tap.focus();
		handoffFocus(el, tap, allVisible);
		expect(document.activeElement).toBe(el);
		expect(document.activeElement).not.toBe(tap);
	});

	it('imperative shape: no-op when the departing control is not the focused one', () => {
		const el = mount(`<button id="close">close</button><button id="next">next</button>`);
		const close = document.getElementById('close')!;
		close.focus();
		const next = document.getElementById('next')!;
		handoffFocus(el, next, allVisible);
		expect(document.activeElement).toBe(close);
	});

	it('drops to the container when the preferred fallback REFUSES focus (inert / hidden)', () => {
		// jsdom cannot reproduce inert (it always focuses), so stub the close
		// button's focus() to no-op — the real-engine shape of a fallback that sits
		// under an inert / hidden ancestor. Focus must still land INSIDE the surface
		// (the tabindex="-1" container), never on <body>.
		const el = mount(`<button id="close">close</button><button id="next">next</button>`);
		el.tabIndex = -1;
		const next = document.getElementById('next')!;
		next.focus();
		next.remove();
		const close = document.getElementById('close') as HTMLButtonElement;
		close.focus = () => {}; // refuses focus, like an inert control
		handoffFocus(el, null, allVisible);
		expect(document.activeElement).toBe(el);
		expect(document.activeElement).not.toBe(document.body);
	});

	it('falls back to the container itself when it has no tabbable control', () => {
		// Mid-load a viewer can have no focusable control yet; the container is
		// `tabindex="-1"` so focus still lands inside the surface, never on <body>.
		const el = mount(`<span>loading…</span>`);
		el.tabIndex = -1;
		expect(document.activeElement).toBe(document.body);
		handoffFocus(el, null, allVisible);
		expect(document.activeElement).toBe(el);
	});
});
