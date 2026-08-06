import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
	acquire,
	isBlockedByModal,
	isViewerFrontmost,
	hasForeignEscapeOwner,
	noteEscapeConsumedByViewer,
	isEscapeConsumedByViewer,
	VIEWER_ROOT_CLASS,
	__resetViewerBackdropForTests,
} from './viewerBackdrop';

// jsdom has no layout engine, so the production visibility predicate behind
// `paneFocusables` (offsetParent / getClientRects) reports EVERYTHING hidden.
// Stub `getClientRects` so focus-handoff assertions exercise the real
// selection path instead of always seeing an empty candidate list.
const realGetClientRects = HTMLElement.prototype.getClientRects;

/** A direct child of `<body>` — the shape a portaled viewer root has. */
function bodyChild(id: string, html = ''): HTMLElement {
	const el = document.createElement('div');
	el.id = id;
	el.innerHTML = html;
	document.body.appendChild(el);
	return el;
}

/** A body-level `<dialog>` standing in for a `showModal()` one. */
function openModal(html = ''): HTMLDialogElement {
	const dialog = document.createElement('dialog');
	dialog.innerHTML = html;
	document.body.appendChild(dialog);
	return dialog;
}

/**
 * jsdom implements neither `showModal()` top-layer state nor the `:modal`
 * pseudo-class (it throws on it), so emulate a supporting engine: `dialogs`
 * are the open modal ones, everything else answers normally. Both probes the
 * module makes — `document.querySelectorAll` and `Element.matches` — are
 * covered, and `selectorLog` (when given) records every selector asked for.
 */
function mockOpenModals(
	oracle: Element[] | ((el: Element) => boolean),
	selectorLog?: string[],
): void {
	const isModal = (el: Element) =>
		typeof oracle === 'function' ? oracle(el) : oracle.includes(el);
	const realQueryAll = document.querySelectorAll.bind(document);
	vi.spyOn(document, 'querySelectorAll').mockImplementation((selector: string) => {
		selectorLog?.push(selector);
		if (selector !== 'dialog:modal') return realQueryAll(selector);
		// Evaluated per call, so an oracle keyed on live state (a dialog that
		// opens mid-lease) is reflected the way a real engine would.
		return Array.from(realQueryAll('dialog')).filter(isModal) as unknown as NodeListOf<Element>;
	});
	const realMatches = Element.prototype.matches;
	vi.spyOn(Element.prototype, 'matches').mockImplementation(function (
		this: Element,
		selector: string,
	) {
		selectorLog?.push(selector);
		if (selector !== 'dialog:modal') return realMatches.call(this, selector);
		return realMatches.call(this, 'dialog') && isModal(this);
	});
}

/**
 * Observer-lifecycle probe: counts constructions / observe / disconnect so a
 * leaked or duplicated observer is visible (a leak is otherwise silent — at
 * zero leases the desired inert set is empty either way).
 */
const observerLog: { observed: number; disconnected: number }[] = [];
class TrackingMutationObserver extends MutationObserver {
	#record = { observed: 0, disconnected: 0 };
	constructor(callback: MutationCallback) {
		super(callback);
		observerLog.push(this.#record);
	}
	observe(target: Node, options?: MutationObserverInit) {
		this.#record.observed++;
		super.observe(target, options);
	}
	disconnect() {
		this.#record.disconnected++;
		super.disconnect();
	}
}

/** The exact set of body-child ids currently carrying `inert`. */
function inertIds(): string[] {
	return Array.from(document.body.children)
		.filter((el) => el.hasAttribute('inert'))
		.map((el) => el.id)
		.sort();
}

/** Let the shared MutationObserver deliver its childList records. */
function flushObserver(): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, 0));
}

beforeEach(() => {
	HTMLElement.prototype.getClientRects = function () {
		return [{}] as unknown as DOMRectList;
	};
	document.body.innerHTML = '';
});

afterEach(() => {
	__resetViewerBackdropForTests();
	document.body.innerHTML = '';
	HTMLElement.prototype.getClientRects = realGetClientRects;
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

describe('acquire / release', () => {
	it('inerts every body child except the exempt root, and restores on release', () => {
		const app = bodyChild('app');
		const viewer = bodyChild('viewer');

		const lease = acquire(viewer);
		expect(inertIds()).toEqual(['app']);
		expect(lease.exemptRoot).toBe(viewer);
		expect(viewer.hasAttribute('inert')).toBe(false);

		expect(lease.release()).toEqual({ released: true, stackEmpty: true });
		expect(inertIds()).toEqual([]);
	});

	it('is a no-op on repeat release', () => {
		const app = bodyChild('app');
		const viewer = bodyChild('viewer');

		const lease = acquire(viewer);
		expect(lease.release()).toEqual({ released: true, stackEmpty: true });
		expect(lease.release()).toEqual({ released: false, stackEmpty: true });
		expect(app.hasAttribute('inert')).toBe(false);
	});

	it('refcounts: an inner lease keeps the backdrop up until the outer releases', () => {
		bodyChild('app');
		const first = bodyChild('first');
		const second = bodyChild('second');

		const a = acquire(first);
		const b = acquire(second);
		// Only the FRONTMOST root stays interactive — otherwise two viewers
		// would inert each other.
		expect(inertIds()).toEqual(['app', 'first']);

		expect(b.release()).toEqual({ released: true, stackEmpty: false });
		expect(inertIds()).toEqual(['app', 'second']);

		expect(a.release()).toEqual({ released: true, stackEmpty: true });
		expect(inertIds()).toEqual([]);
	});

	it('recomputes rather than undoing its own writes on out-of-order release', () => {
		bodyChild('app');
		const first = bodyChild('first');
		const second = bodyChild('second');

		const a = acquire(first);
		const b = acquire(second);

		// Releasing the BACKGROUND lease must leave `first` inert — `second` is
		// still frontmost, so the desired set is unchanged.
		expect(a.release()).toEqual({ released: true, stackEmpty: false });
		expect(inertIds()).toEqual(['app', 'first']);

		expect(b.release()).toEqual({ released: true, stackEmpty: true });
		expect(inertIds()).toEqual([]);
	});

	it('leaves pre-existing inert alone (records only what it set)', () => {
		const app = bodyChild('app');
		const other = bodyChild('other');
		other.setAttribute('inert', '');
		const viewer = bodyChild('viewer');

		const lease = acquire(viewer);
		expect(inertIds()).toEqual(['app', 'other']);

		lease.release();
		// `app` is ours to clear; `other` was inert before we arrived.
		expect(app.hasAttribute('inert')).toBe(false);
		expect(other.hasAttribute('inert')).toBe(true);
	});

	it('never inerts the exempt root, even across stack transitions', () => {
		const app = bodyChild('app');
		const first = bodyChild('first');
		const second = bodyChild('second');

		const a = acquire(first);
		expect(first.hasAttribute('inert')).toBe(false);
		// Paired positive assertion, so a "never write inert at all" mutant can't
		// satisfy the negative ones.
		expect(app.hasAttribute('inert')).toBe(true);

		const b = acquire(second);
		expect(second.hasAttribute('inert')).toBe(false);
		expect(first.hasAttribute('inert')).toBe(true);

		b.release();
		expect(first.hasAttribute('inert')).toBe(false);
		expect(second.hasAttribute('inert')).toBe(true);
		a.release();
		expect(inertIds()).toEqual([]);
	});

	it('warns when the exempt root is not a direct child of <body>', () => {
		const app = bodyChild('app', '<div id="nested"></div>');
		const nested = app.querySelector<HTMLElement>('#nested')!;
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

		const lease = acquire(nested);
		expect(warn).toHaveBeenCalled();
		// The containing body child is inerted, which is exactly why it's wrong.
		expect(app.hasAttribute('inert')).toBe(true);
		lease.release();
	});
});

describe('shared child-list observer', () => {
	it('inerts a sibling appended after acquisition', async () => {
		bodyChild('app');
		const viewer = bodyChild('viewer');
		const lease = acquire(viewer);

		bodyChild('late');
		await flushObserver();
		expect(inertIds()).toEqual(['app', 'late']);

		lease.release();
		expect(inertIds()).toEqual([]);
	});

	it('keeps observing across a nested acquire/release pair', async () => {
		bodyChild('app');
		const first = bodyChild('first');
		const second = bodyChild('second');

		const a = acquire(first);
		const b = acquire(second);
		b.release();

		bodyChild('late');
		await flushObserver();
		expect(inertIds()).toEqual(['app', 'late', 'second']);
		a.release();
	});

	it('constructs ONE observer for the whole stack and disconnects it at zero leases', () => {
		observerLog.length = 0;
		vi.stubGlobal('MutationObserver', TrackingMutationObserver);
		const seen = observerLog;

		bodyChild('app');
		const a = acquire(bodyChild('first'));
		const b = acquire(bodyChild('second'));
		// ONE observer instance for the whole stack, with two observe targets:
		// `<body>` (the child set) and `<html>` (to catch a body swap).
		expect(seen.length).toBe(1);
		expect(seen[0].observed).toBe(2);

		b.release();
		expect(seen[0].disconnected).toBe(0);
		a.release();
		expect(seen[0].disconnected).toBe(1);
	});

	it('leaves no observer activity after the last release', async () => {
		observerLog.length = 0;
		vi.stubGlobal('MutationObserver', TrackingMutationObserver);
		bodyChild('app');
		const viewer = bodyChild('viewer');
		const lease = acquire(viewer);
		lease.release();

		bodyChild('late');
		await flushObserver();
		expect(inertIds()).toEqual([]);
		// The DOM result alone can't tell a torn-down observer from a leaked one
		// (an empty stack reconciles to nothing either way), so assert teardown.
		expect(observerLog[0].disconnected).toBe(1);
	});

	// Asserts the OUTCOME (the inert set is undisturbed), not the reconcile
	// count — a broader observer filter would be wasteful, not wrong, since
	// reconcile is idempotent.
	it('is undisturbed by mutations that cannot change the body-child set', async () => {
		const app = bodyChild('app', '<details id="det"><summary>s</summary></details>');
		const viewer = bodyChild('viewer');
		const lease = acquire(viewer);
		const before = inertIds();

		// Nested childList + a non-dialog `open` toggle deep in the app: neither
		// can add or remove a body child, so neither needs a reconcile.
		app.appendChild(document.createElement('button'));
		document.getElementById('det')?.setAttribute('open', '');
		await flushObserver();

		expect(inertIds()).toEqual(before);
		expect(viewer.hasAttribute('inert')).toBe(false);
		lease.release();
	});

	it('re-arms on a replaced <body> without needing another acquire', async () => {
		const firstBody = document.body;
		bodyChild('app');
		const viewer = bodyChild('viewer');
		const lease = acquire(viewer);

		const nextBody = document.createElement('body');
		firstBody.replaceWith(nextBody);
		expect(document.body).toBe(nextBody);
		nextBody.appendChild(viewer);
		const app2 = bodyChild('app2');
		await flushObserver();

		// The swap itself re-arms the observer — the lease may never see another
		// acquire, so waiting for one would leave the backdrop watching a
		// detached body.
		expect(app2.hasAttribute('inert')).toBe(true);

		bodyChild('late');
		await flushObserver();
		expect(document.getElementById('late')?.hasAttribute('inert')).toBe(true);
		expect(viewer.hasAttribute('inert')).toBe(false);
		lease.release();
	});
});

describe('focus handoff', () => {
	it('focuses the new frontmost root’s first tabbable descendant', () => {
		bodyChild('app');
		const first = bodyChild(
			'first',
			'<div id="pad" tabindex="-1"></div><button id="a">a</button><button id="b">b</button>',
		);
		const second = bodyChild('second', '<button id="c">c</button>');

		const a = acquire(first);
		const b = acquire(second);
		(document.getElementById('c') as HTMLElement).focus();

		b.release();
		expect(document.activeElement?.id).toBe('a');
		a.release();
	});

	it('does not steal focus when a BACKGROUND lease is released', () => {
		bodyChild('app');
		const first = bodyChild('first', '<button id="a">a</button>');
		// TWO controls, with focus on the second: a handoff that ignored
		// `wasFrontmost` would focus this root's FIRST tabbable, which is a
		// different element — so the assertion can actually catch it.
		const second = bodyChild('second', '<button id="c">c</button><button id="c2">c2</button>');

		const a = acquire(first);
		const b = acquire(second);
		const c2 = document.getElementById('c2') as HTMLElement;
		c2.focus();

		a.release();
		expect(document.activeElement).toBe(c2);
		b.release();
	});

	it('does not hand off when the stack empties', () => {
		const app = bodyChild('app', '<button id="a">a</button>');
		const viewer = bodyChild('viewer', '<button id="v">v</button>');
		const lease = acquire(viewer);
		const v = document.getElementById('v') as HTMLElement;
		v.focus();

		expect(lease.release()).toEqual({ released: true, stackEmpty: true });
		// Caller restores its own invoker on `stackEmpty`; we must not have moved
		// focus ourselves — it stays exactly where the viewer left it.
		expect(document.activeElement).toBe(v);
		expect(app.contains(document.activeElement)).toBe(false);
	});

	it('leaves focus alone when it sits outside the closing viewer', () => {
		bodyChild('app');
		const first = bodyChild('first', '<button id="a">a</button>');
		const second = bodyChild('second', '<button id="c">c</button>');
		const elsewhere = bodyChild('elsewhere', '<button id="e">e</button>');

		const a = acquire(first);
		const b = acquire(second);
		// Something layered over the viewer (a native modal, say) took focus.
		const e = document.getElementById('e') as HTMLElement;
		e.focus();

		b.release();
		expect(document.activeElement).toBe(e);
		a.release();
	});

	it('defers to a native modal nested inside the closing viewer', () => {
		bodyChild('app');
		const first = bodyChild('first', '<button id="a">a</button>');
		const second = bodyChild('second', '<button id="c">c</button>');
		// A dialog opened FROM the closing viewer lives inside it, so plain
		// containment would read "focus is ours" and yank it out of the dialog.
		const dialog = document.createElement('dialog');
		dialog.innerHTML = '<button id="d">d</button>';
		second.appendChild(dialog);
		mockOpenModals([dialog]);

		const a = acquire(first);
		const b = acquire(second);
		const d = document.getElementById('d') as HTMLElement;
		d.focus();

		b.release();
		expect(document.activeElement).toBe(d);
		a.release();
	});

	it('defers to a nested native modal even when focus is adrift on <body>', () => {
		bodyChild('app');
		const first = bodyChild('first', '<button id="a">a</button>');
		const second = bodyChild('second', '<button id="c">c</button>');
		const dialog = document.createElement('dialog');
		dialog.innerHTML = '<button id="d">d</button>';
		second.appendChild(dialog);
		mockOpenModals([dialog]);

		const a = acquire(first);
		const b = acquire(second);
		(document.getElementById('d') as HTMLElement).focus();
		(document.getElementById('d') as HTMLElement).blur();
		expect(document.activeElement).toBe(document.body);

		b.release();
		// The dialog is still open and still on top — adrift focus is not a
		// licence to pull it into the viewer beneath.
		expect(document.activeElement).toBe(document.body);
		a.release();
	});

	it('defers to an open native modal that sits OUTSIDE the closing viewer', () => {
		bodyChild('app');
		const first = bodyChild('first', '<button id="a">a</button>');
		const second = bodyChild('second', '<button id="c">c</button>');
		// Body-level dialog: neither inside the closing viewer nor containing
		// the (adrift) active element — but still the top layer.
		const dialog = openModal('<button id="d">d</button>');
		mockOpenModals([dialog]);

		const a = acquire(first);
		const b = acquire(second);
		expect(document.activeElement).toBe(document.body);

		b.release();
		expect(document.activeElement).toBe(document.body);
		a.release();
	});

	it('hands off when focus is adrift on <body> after teardown', () => {
		bodyChild('app');
		const first = bodyChild('first', '<button id="a">a</button>');
		const second = bodyChild('second', '<button id="c">c</button>');

		const a = acquire(first);
		const b = acquire(second);
		(document.getElementById('c') as HTMLElement).focus();
		(document.getElementById('c') as HTMLElement).blur();
		expect(document.activeElement).toBe(document.body);

		b.release();
		expect(document.activeElement?.id).toBe('a');
		a.release();
	});

	it('focuses the new frontmost root itself when it has no tabbable descendant', () => {
		bodyChild('app');
		const first = bodyChild('first', '<div id="static">no controls</div>');
		const second = bodyChild('second', '<button id="c">c</button>');

		const a = acquire(first);
		const b = acquire(second);
		(document.getElementById('c') as HTMLElement).focus();

		b.release();
		// Focus must not be left on the closing viewer — it's about to unmount,
		// and the caller only restores its invoker on `stackEmpty`.
		expect(document.activeElement).toBe(first);
		expect(first.getAttribute('tabindex')).toBe('-1');
		a.release();
	});
});

describe('isViewerFrontmost', () => {
	it('tracks the top of the stack', () => {
		const first = bodyChild('first');
		const second = bodyChild('second');

		expect(isViewerFrontmost(first)).toBe(false);
		const a = acquire(first);
		expect(isViewerFrontmost(first)).toBe(true);

		const b = acquire(second);
		expect(isViewerFrontmost(first)).toBe(false);
		expect(isViewerFrontmost(second)).toBe(true);

		b.release();
		expect(isViewerFrontmost(first)).toBe(true);
		a.release();
		expect(isViewerFrontmost(first)).toBe(false);
	});
});

describe('isBlockedByModal', () => {
	it('returns false on an empty stack', () => {
		const app = bodyChild('app', '<button id="a">a</button>');
		expect(isBlockedByModal()).toBe(false);
		expect(isBlockedByModal(app.querySelector('#a'))).toBe(false);
		expect(isBlockedByModal(null)).toBe(false);
	});

	it('blocks an owner outside the frontmost viewer, not one inside it', () => {
		const app = bodyChild('app', '<button id="a">a</button>');
		const viewer = bodyChild('viewer', '<button id="v">v</button>');
		const lease = acquire(viewer);

		expect(isBlockedByModal(app.querySelector('#a'))).toBe(true);
		expect(isBlockedByModal(viewer.querySelector('#v'))).toBe(false);
		expect(isBlockedByModal(viewer)).toBe(false);
		// An owner-less caller can't prove it's the frontmost surface.
		expect(isBlockedByModal()).toBe(true);

		lease.release();
		expect(isBlockedByModal(app.querySelector('#a'))).toBe(false);
	});

	it('blocks against a BACKGROUND viewer only via the frontmost root', () => {
		const first = bodyChild('first', '<button id="a">a</button>');
		const second = bodyChild('second', '<button id="c">c</button>');
		const a = acquire(first);
		const b = acquire(second);

		expect(isBlockedByModal(first.querySelector('#a'))).toBe(true);
		expect(isBlockedByModal(second.querySelector('#c'))).toBe(false);

		b.release();
		expect(isBlockedByModal(first.querySelector('#a'))).toBe(false);
		a.release();
	});

	it('derives from lease state, not from DOM inert', () => {
		const app = bodyChild('app', '<button id="a">a</button>');
		app.setAttribute('inert', '');
		expect(isBlockedByModal(app.querySelector('#a'))).toBe(false);
	});

	describe('native dialog branch', () => {
		it('blocks an owner outside the open modal dialog, not one inside it', () => {
			const app = bodyChild('app', '<button id="a">a</button>');
			const dialog = openModal('<button id="d">d</button>');
			mockOpenModals([dialog]);

			expect(isBlockedByModal(app.querySelector('#a'))).toBe(true);
			expect(isBlockedByModal(dialog.querySelector('#d'))).toBe(false);
		});

		it('takes precedence over a held viewer lease (top layer wins)', () => {
			const viewer = bodyChild('viewer', '<button id="v">v</button>');
			const dialog = openModal('<button id="d">d</button>');
			mockOpenModals([dialog]);
			const lease = acquire(viewer);

			// A `showModal()` dialog opened OVER the viewer is above it, so the
			// viewer's own controls are blocked while the dialog's are not.
			expect(isBlockedByModal(dialog.querySelector('#d'))).toBe(false);
			expect(isBlockedByModal(viewer.querySelector('#v'))).toBe(true);

			vi.restoreAllMocks();
			expect(isBlockedByModal(viewer.querySelector('#v'))).toBe(false);
			lease.release();
		});

		it('exempts EVERY open modal, since top-layer order is not observable', () => {
			bodyChild('app');
			const lower = openModal('<button id="lo">lo</button>');
			const upper = openModal('<button id="hi">hi</button>');
			mockOpenModals([lower, upper]);

			expect(isBlockedByModal(upper.querySelector('#hi'))).toBe(false);
			expect(isBlockedByModal(lower.querySelector('#lo'))).toBe(false);
			expect(isBlockedByModal(document.getElementById('app'))).toBe(true);
		});

		it('never inerts an open modal dialog (an inert dialog is dead)', () => {
			bodyChild('app');
			const dialog = openModal('<button id="d">d</button>');
			// A second, NON-modal dialog: it must still be inerted, so a lazy
			// "skip every <dialog>" implementation can't pass.
			const inactive = openModal();
			inactive.id = 'inactive';
			const viewer = bodyChild('viewer');
			mockOpenModals([dialog]);

			const lease = acquire(viewer);
			// A modal dialog escapes an inert ANCESTOR, but `inert` ON the dialog
			// still disables it — so it must stay out of the backdrop's set.
			expect(dialog.hasAttribute('inert')).toBe(false);
			expect(inactive.hasAttribute('inert')).toBe(true);
			expect(document.getElementById('app')?.hasAttribute('inert')).toBe(true);
			lease.release();
		});

		it('does not inert a genuinely modal dialog when `:modal` is unsupported', () => {
			bodyChild('app');
			const dialog = openModal('<button id="d">d</button>');
			const closed = openModal();
			closed.id = 'closed';
			const viewer = bodyChild('viewer');

			// jsdom's real behaviour: `:modal` throws. The arbitration fallback
			// ("no modal") is safe, but applying it to the MUTATOR would write
			// `inert` onto a live `showModal()` dialog and kill it.
			dialog.showModal();
			const lease = acquire(viewer);

			expect(dialog.open).toBe(true);
			expect(dialog.hasAttribute('inert')).toBe(false);
			// A CLOSED dialog is still just another body child.
			expect(closed.hasAttribute('inert')).toBe(true);

			dialog.close();
			lease.release();
		});

		it('un-inerts a body-level dialog that goes modal DURING a lease', async () => {
			bodyChild('app');
			const dialog = openModal('<button id="d">d</button>');
			const viewer = bodyChild('viewer');
			// Modal-ness tracks the live `open` state, as in a real engine.
			mockOpenModals((el) => el === dialog && dialog.hasAttribute('open'));

			const lease = acquire(viewer);
			// Closed at acquisition: it's just another body child, correctly inert.
			expect(dialog.hasAttribute('inert')).toBe(true);

			dialog.showModal();
			await flushObserver();

			// `showModal()` is an attribute change, not a childList one — without
			// open-state observation the dialog would stay inert, i.e. dead.
			expect(dialog.open).toBe(true);
			expect(dialog.hasAttribute('inert')).toBe(false);
			expect(isBlockedByModal(dialog.querySelector('#d'))).toBe(false);

			dialog.close();
			lease.release();
		});

		it('probes `dialog:modal`, never `[open]`', () => {
			const app = bodyChild('app', '<button id="a">a</button>');
			const dialog = openModal();
			dialog.setAttribute('open', '');

			// jsdom has no `:modal` pseudo-class, so asserting "a declarative
			// `<dialog open>` doesn't block" would pass trivially through the
			// unsupported-selector fallback. Emulate a supporting engine instead
			// (where `show()` / declarative-open dialogs do NOT match `:modal`)
			// and assert the selector the module actually asks for.
			const selectors: string[] = [];
			mockOpenModals([], selectors);

			expect(isBlockedByModal(app.querySelector('#a'))).toBe(false);
			expect(selectors).toContain('dialog:modal');
			expect(selectors.some((s) => s.includes('[open]'))).toBe(false);
		});

		it('falls back to no native modal when `:modal` is unsupported', () => {
			const app = bodyChild('app', '<button id="a">a</button>');
			// A declaratively-open, NON-modal dialog: an implementation that
			// answered the unsupported `:modal` with an `[open]` query would find
			// this and wrongly block, so the fixture can catch that regression.
			const dialog = openModal('<button id="d">d</button>');
			dialog.setAttribute('open', '');

			const realQueryAll = document.querySelectorAll.bind(document);
			const probe = vi
				.spyOn(document, 'querySelectorAll')
				.mockImplementation((selector: string) => {
					if (selector === 'dialog:modal') {
						throw new SyntaxError('unsupported pseudo-class');
					}
					return realQueryAll(selector);
				});

			expect(isBlockedByModal(app.querySelector('#a'))).toBe(false);
			// Guards against a "return false without asking" implementation.
			expect(probe).toHaveBeenCalledWith('dialog:modal');
			// ...and against any `[open]` fallback: the open dialog must not count.
			expect(isBlockedByModal(dialog.querySelector('#d'))).toBe(false);
		});
	});
});

describe('event-scoped Escape consumption (TASK-2448 / BUG-2441)', () => {
	const esc = () => new KeyboardEvent('keydown', { key: 'Escape' });

	it('survives the release of the lease that recorded it', () => {
		// The whole point: the mark answers for a viewer that no longer exists.
		const app = bodyChild('app', '<button id="a">a</button>');
		const viewer = bodyChild('viewer');
		const lease = acquire(viewer);
		const event = esc();

		noteEscapeConsumedByViewer(event);
		lease.release();

		expect(isBlockedByModal(app, null)).toBe(false); // live state: nothing in front
		expect(isBlockedByModal(app, event)).toBe(true); // ...but this press is spent
	});

	it('is scoped to ONE event — the next press is unaffected', () => {
		const app = bodyChild('app');
		const consumed = esc();
		noteEscapeConsumedByViewer(consumed);

		expect(isBlockedByModal(app, consumed)).toBe(true);
		expect(isBlockedByModal(app, esc())).toBe(false);
	});

	it('blocks an owner INSIDE the closing viewer too', () => {
		// A viewer that has spent the press has spent it for everyone: the
		// surface that won it is being torn down, so "inside it" is not a licence
		// to act on the same key.
		const viewer = bodyChild('viewer', '<button id="v">v</button>');
		const lease = acquire(viewer);
		const event = esc();
		noteEscapeConsumedByViewer(event);

		expect(isBlockedByModal(viewer.querySelector('#v'), event)).toBe(true);
		lease.release();
	});

	it('changes NOTHING with an unmarked event, at any lease state', () => {
		// The empty-stack contract, stated as an equivalence: passing an event
		// nobody marked must give the same answer as not passing one at all.
		const app = bodyChild('app', '<button id="a">a</button>');
		const owner = app.querySelector('#a');
		const event = esc();

		expect(isBlockedByModal(owner, event)).toBe(isBlockedByModal(owner));
		expect(isBlockedByModal(owner, event)).toBe(false);

		const viewer = bodyChild('viewer');
		const lease = acquire(viewer);
		expect(isBlockedByModal(owner, event)).toBe(isBlockedByModal(owner));
		expect(isBlockedByModal(owner, event)).toBe(true);
		expect(isBlockedByModal(viewer, event)).toBe(isBlockedByModal(viewer));
		expect(isBlockedByModal(viewer, event)).toBe(false);
		lease.release();
	});

	it('reports the mark directly, and treats a missing event as unmarked', () => {
		const event = esc();
		expect(isEscapeConsumedByViewer(event)).toBe(false);
		noteEscapeConsumedByViewer(event);
		expect(isEscapeConsumedByViewer(event)).toBe(true);
		expect(isEscapeConsumedByViewer(null)).toBe(false);
		expect(isEscapeConsumedByViewer(undefined)).toBe(false);
	});
});

describe('hasForeignEscapeOwner', () => {
	// The shared form of the existence check the two route ESC guards used to
	// hand-roll (TASK-2429). It answers a NARROWER question than
	// `isBlockedByModal`: not "is something in front of me" but "does a surface
	// that owns Escape ITSELF exist" — so a driver of the escape stack knows
	// whether to stand down entirely.

	/** The shape a portaled viewer root has, including the marker class. */
	function viewerRoot(): HTMLElement {
		const el = bodyChild('viewer');
		el.setAttribute('role', 'dialog');
		el.classList.add(VIEWER_ROOT_CLASS);
		return el;
	}

	/**
	 * Emulate an engine where `dialog:modal` IS supported (jsdom throws on it —
	 * probed, not assumed), with `modals` as the open modal dialogs. Intercepts
	 * ANY selector starting with `dialog:modal` — the module asks it both
	 * standalone and combined with the ARIA branch, and delegating the combined
	 * form's tail unconditionally would turn the standalone form back into a
	 * throwing `dialog:modal` query and silently drop the caller onto the
	 * unsupported-engine path.
	 */
	function mockModalSupport(modals: Element[]): void {
		const real = document.querySelector.bind(document);
		vi.spyOn(document, 'querySelector').mockImplementation((selector: string) => {
			if (!selector.startsWith('dialog:modal')) return real(selector);
			if (modals[0]) return modals[0];
			// Shape-agnostic (TASK-2430): the helper may ask `dialog:modal` on its
			// own or combined with the ARIA branch. Delegating `slice(indexOf(',')
			// + 1)` unconditionally turned the standalone form back into
			// `real('dialog:modal')`, which THROWS in jsdom and pushed the helper
			// down its unsupported-engine fallback — the opposite of what a test
			// named "on a supporting engine" is asking about.
			const comma = selector.indexOf(',');
			return comma === -1 ? null : real(selector.slice(comma + 1).trim());
		});
	}

	it('is false with nothing open, so today’s guards are unchanged', () => {
		bodyChild('app', '<button id="a">a</button>');
		expect(hasForeignEscapeOwner()).toBe(false);
	});

	it('is true for an ARIA sheet — the shipped BottomSheet / DockedSheet', () => {
		// The regression fence for the branch that must NOT be dropped: both are
		// `role="dialog"` Escape owners with no escape-stack registration, so a
		// guard that stopped seeing them would let one press close two layers.
		bodyChild('sheet').setAttribute('role', 'dialog');
		expect(hasForeignEscapeOwner()).toBe(true);
	});

	it('is false for the pane’s own mobile overlay', () => {
		const pane = bodyChild('pane');
		pane.setAttribute('role', 'dialog');
		pane.classList.add('item-pane');
		expect(hasForeignEscapeOwner()).toBe(false);
	});

	it('is false for the attachment viewer: its Escape is on the stack', () => {
		// The whole reason this helper exists. A guard that treated the viewer as
		// a foreign modal would return before running the stack, and Escape would
		// close nothing at all.
		viewerRoot();
		expect(hasForeignEscapeOwner()).toBe(false);
	});

	it('still sees a sheet when a viewer ELEMENT exists but holds no lease', () => {
		// The exclusion is targeted at the viewer element, not a blanket "a viewer
		// is in the DOM, so nothing else counts". With no lease held, the viewer
		// is not in front of anything — a mid-teardown viewer is exactly this
		// state — and the sheet owns its Escape as it always did.
		viewerRoot();
		bodyChild('sheet').setAttribute('role', 'dialog');
		expect(hasForeignEscapeOwner()).toBe(true);
	});

	it('stops seeing a sheet once a viewer LEASE is frontmost (TASK-2430)', () => {
		// THE DEAD-KEY FENCE. Once the sheets consult `isBlockedByModal` (3b),
		// a sheet under a frontmost viewer stands down — so if this still answered
		// `true`, the route driver would return, the sheet would decline, and the
		// viewer's Escape (which lives ONLY on the stack) would run nowhere.
		// Nobody would own the key.
		const sheet = bodyChild('sheet');
		sheet.setAttribute('role', 'dialog');
		expect(hasForeignEscapeOwner()).toBe(true);

		const lease = acquire(viewerRoot());
		expect(hasForeignEscapeOwner()).toBe(false);

		// Released → the sheet is the front layer again and reclaims the key.
		lease.release();
		expect(hasForeignEscapeOwner()).toBe(true);
	});

	it('is lease-aware on a `:modal`-SUPPORTING engine too, not just the fallback', () => {
		// jsdom throws on `:modal`, so every test in this describe that does not
		// install an emulation exercises the FALLBACK branch — which would leave
		// the branch real browsers take unfenced for the LEASE case specifically.
		// Emulating support needs BOTH probes mocked, not just
		// `querySelector`: `acquire()` runs `reconcile()`, which calls
		// `el.matches('dialog:modal')` on body children, and an unmocked throw
		// there latches `modalSelectorSupported = false` and silently drops the
		// helper onto the fallback path mid-test. `mockOpenModals` covers both.
		const modals: Element[] = [];
		mockOpenModals((el) => modals.includes(el));
		// `hasForeignEscapeOwner` asks `querySelector`, which `mockOpenModals`
		// does not intercept — route it to the same oracle.
		const realQuery = document.querySelector.bind(document);
		vi.spyOn(document, 'querySelector').mockImplementation((selector: string) => {
			if (selector !== 'dialog:modal') return realQuery(selector);
			return modals[0] ?? null;
		});

		const sheet = bodyChild('sheet');
		sheet.setAttribute('role', 'dialog');
		expect(hasForeignEscapeOwner()).toBe(true);

		const lease = acquire(viewerRoot());
		expect(hasForeignEscapeOwner()).toBe(false);

		// ...and a real `showModal()` dialog still wins over the held lease.
		modals.push(openModal());
		expect(hasForeignEscapeOwner()).toBe(true);
		lease.release();
	});

	it('STILL sees a sheet nested INSIDE the frontmost viewer', () => {
		// The containment half of the lease-aware branch. A sheet that lives
		// inside the viewer root is IN FRONT of the viewer's own content, so it
		// genuinely owns its Escape — and `isBlockedByModal(sheetEl)` agrees
		// (contained by the front root ⇒ not blocked). A blanket "a lease exists
		// ⇒ no ARIA owner" would let the route driver run the stack and close the
		// viewer out from under the sheet it is showing: two layers, one press.
		const viewer = viewerRoot();
		const nested = document.createElement('div');
		nested.setAttribute('role', 'dialog');
		viewer.appendChild(nested);
		acquire(viewer);

		expect(hasForeignEscapeOwner()).toBe(true);

		// ...while a sheet BEHIND the same viewer is still invisible to it.
		nested.remove();
		bodyChild('sheet').setAttribute('role', 'dialog');
		expect(hasForeignEscapeOwner()).toBe(false);
	});

	it('does NOT apply the liveness rule to the `dialog[open]` fallback', () => {
		// The containment rule is deliberately ARIA-only. Nothing in the app
		// guards a native `<dialog>`, so unlike a sheet it has not stood down for
		// the viewer and does still own Escape — and on this branch a real
		// `showModal()` dialog is indistinguishable from a non-modal one, so
		// letting the lease out-rank it would fire the browser's native `cancel`
		// AND run the stack: two layers on one press.
		const dialog = openModal();
		dialog.setAttribute('open', '');
		expect(hasForeignEscapeOwner()).toBe(true);

		const lease = acquire(viewerRoot());
		expect(hasForeignEscapeOwner()).toBe(true);
		lease.release();
	});

	it('falls back to `dialog[open]` where `:modal` is unsupported', () => {
		// jsdom is that engine (it throws on the pseudo-class), so this is the
		// path every other test here runs on. The fallback is deliberately the
		// PRE-TASK-2429 selector: where the narrower question can't be asked, the
		// answer is exactly today's behaviour, never something wider.
		const dialog = openModal();
		dialog.setAttribute('open', '');
		expect(hasForeignEscapeOwner()).toBe(true);
	});

	it('does not count a CLOSED native <dialog> in the fallback path', () => {
		// `Modal.svelte` keeps its native <dialog> mounted at all times and drives
		// it with showModal()/close(), so a fallback that dropped the `[open]`
		// qualifier would report a foreign Escape owner on every page that merely
		// HAS a Modal — swallowing the viewer's Escape everywhere.
		openModal();
		expect(hasForeignEscapeOwner()).toBe(false);
	});

	it('asks for `dialog:modal` before falling back', () => {
		// Guards against an implementation that simply hard-codes `[open]`.
		const seen: string[] = [];
		const real = document.querySelector.bind(document);
		vi.spyOn(document, 'querySelector').mockImplementation((selector: string) => {
			seen.push(selector);
			if (selector.startsWith('dialog:modal')) throw new SyntaxError('unsupported');
			return real(selector);
		});
		hasForeignEscapeOwner();
		expect(seen.some((s) => s.startsWith('dialog:modal'))).toBe(true);
		expect(seen.some((s) => s.includes('dialog[open]'))).toBe(true);
	});

	it('on a supporting engine, an open NON-modal <dialog> does not count', () => {
		// The one behaviour change from the hand-rolled `dialog[open]` string: a
		// `show()` / declarative-open dialog never owned Escape, and
		// `Modal.svelte` keeps a native <dialog> mounted at all times.
		const dialog = openModal();
		dialog.setAttribute('open', '');
		mockModalSupport([]);
		expect(hasForeignEscapeOwner()).toBe(false);

		// ...while a real `showModal()` one does.
		mockModalSupport([dialog]);
		expect(hasForeignEscapeOwner()).toBe(true);
	});
});
