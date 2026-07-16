import { describe, it, expect } from 'vitest';
import { shouldOpenInPane, type CardClickLike } from './itemCardClick';

// A plain, unmodified left-click — the baseline every test below tweaks a
// single field of.
function click(overrides: Partial<CardClickLike> = {}): CardClickLike {
	return {
		button: 0,
		metaKey: false,
		ctrlKey: false,
		shiftKey: false,
		altKey: false,
		defaultPrevented: false,
		...overrides,
	};
}

describe('shouldOpenInPane', () => {
	it('opens the pane on a plain left-click when onItemOpen is wired', () => {
		expect(shouldOpenInPane(click(), true)).toBe(true);
	});

	// Every other surface (starred / tags / roles / share) renders ItemCard
	// with no `onItemOpen` — those clicks must keep navigating full-page.
	it('falls through to href when no onItemOpen handler is wired', () => {
		expect(shouldOpenInPane(click(), false)).toBe(false);
	});

	it('falls through on cmd/meta-click (full-page popout in a new tab)', () => {
		expect(shouldOpenInPane(click({ metaKey: true }), true)).toBe(false);
	});

	it('falls through on ctrl-click', () => {
		expect(shouldOpenInPane(click({ ctrlKey: true }), true)).toBe(false);
	});

	it('falls through on shift-click', () => {
		expect(shouldOpenInPane(click({ shiftKey: true }), true)).toBe(false);
	});

	it('falls through on alt-click', () => {
		expect(shouldOpenInPane(click({ altKey: true }), true)).toBe(false);
	});

	it('falls through on middle-click (button 1, native new-tab open)', () => {
		expect(shouldOpenInPane(click({ button: 1 }), true)).toBe(false);
	});

	it('falls through on right-click (button 2, context menu / copy-link)', () => {
		expect(shouldOpenInPane(click({ button: 2 }), true)).toBe(false);
	});

	// Defensive backstop: sub-controls (star / PR badge / status cycle / tag
	// chips / reorder menu) already stopPropagation so they never reach this
	// predicate, but if some other handler upstream already prevented the
	// default action, the pane must not double-handle the click.
	it('falls through when the click was already defaultPrevented', () => {
		expect(shouldOpenInPane(click({ defaultPrevented: true }), true)).toBe(false);
	});

	it('a held modifier still wins even without onItemOpen wired (both reasons to fall through)', () => {
		expect(shouldOpenInPane(click({ metaKey: true }), false)).toBe(false);
	});
});
