// Node-project SOURCE guard: "Sign out" sits OUTSIDE the user menu's scrolling
// body (BUG-2985).
//
// The behavioural instrument for this is the e2e leg
// (`e2e/bug-2985-signout-reachable.spec.ts`), which measures the item's box
// against the panel's — the only way to catch it, since an element scrolled out
// of an `overflow: auto` container is still "visible" to a DOM query and to
// Playwright. This guard is the cheap half: it fails the moment the structure
// that makes the leg pass is undone, which is the likely regression — a future
// edit moving Sign out back into the scrolling region, or dropping `bodyScroll`
// so the panel scrolls again.
//
// WHAT IT CANNOT DO: it checks structure, not geometry. A pinned footer tall
// enough to overflow the cap on its own would satisfy every assertion here.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const raw = readFileSync(fileURLToPath(new URL('./TopBar.svelte', import.meta.url)), 'utf8');
// Comments quote the markup below, so strip them or the guard passes on its
// own prose (the trap `itemDetailLinksSurviveFailedRefresh` records).
const code = raw
	.replace(/<!--[\s\S]*?-->/g, '')
	.replace(/\/\*[\s\S]*?\*\//g, '')
	.replace(/(^|\s)\/\/[^\n]*/g, '$1');

const menuSrc = readFileSync(
	fileURLToPath(new URL('../common/Menu.svelte', import.meta.url)),
	'utf8',
).replace(/<!--[\s\S]*?-->/g, '').replace(/\/\*[\s\S]*?\*\//g, '');


/**
 * Index just past the `</div>` that closes the `<div>` containing `from`.
 *
 * Counts nesting, because the naive "next `</div>`" reading is what made the
 * first version of the ordering leg pass against its own mutant. Self-closing
 * tags do not occur in this block; `<div ... />` would need handling if they
 * ever did.
 */
function matchingCloseIndex(src: string, from: number): number {
	const open = src.lastIndexOf('<div', from);
	if (open < 0) return -1;
	let depth = 0;
	const tag = /<div\b|<\/div>/g;
	tag.lastIndex = open;
	let m: RegExpExecArray | null;
	while ((m = tag.exec(src))) {
		depth += m[0] === '</div>' ? -1 : 1;
		if (depth === 0) return m.index + m[0].length;
	}
	return -1;
}

describe('the user menu', () => {
	it('hands scrolling to its own body instead of the panel', () => {
		expect(code).toContain('bodyScroll');
		expect(code).toContain('user-dropdown-body');
	});

	it('closes the scrolling body BEFORE the divider and Sign out', () => {
		// The order is the fix: Sign out INSIDE `.user-dropdown-body` is the
		// pre-fix layout with extra markup.
		//
		// MATCHED BY NESTING, not by the next `</div>`. The first version of this
		// leg took `indexOf('</div>', …'Connect a project')` as the body's close
		// and SURVIVED the mutant that moves Sign out back inside — that index
		// landed on the divider's own closing tag, which precedes Sign out in
		// both layouts, so the comparison was true either way. The mutant is
		// faithful; the instrument was not.
		const bodyOpen = code.indexOf('class="user-dropdown-body"');
		expect(bodyOpen, 'no scrolling body').toBeGreaterThan(-1);
		expect(matchingCloseIndex(code, bodyOpen), 'body never closes').toBeGreaterThan(-1);
		const signOut = code.indexOf('Sign out');
		expect(signOut).toBeGreaterThan(bodyOpen);
		expect(signOut, 'Sign out is inside the scrolling body').toBeGreaterThan(
			matchingCloseIndex(code, bodyOpen),
		);
	});

	it('gives the body the min-height that lets it shrink', () => {
		// `min-height: 0` is load-bearing: a flex item defaults to `min-height:
		// auto` and refuses to shrink below its content, which would push the
		// pinned row back out of the panel — the bug, restored, with the markup
		// looking correct.
		expect(code).toMatch(/\.user-dropdown-body\s*\{[^}]*min-height:\s*0/);
		expect(code).toMatch(/\.user-dropdown\s*\{[^}]*min-height:\s*0/);
	});
});

describe('the Menu primitive', () => {
	it('stops scrolling, and still CLIPS, under bodyScroll', () => {
		// `overflow: hidden`, not `visible`: a too-tall menu must still be
		// clipped to the panel, or it paints over the page with no way to reach
		// the rest.
		expect(menuSrc).toMatch(/\.menu-panel\.body-scroll\s*\{[^}]*overflow:\s*hidden/);
		expect(menuSrc).toMatch(/\.menu-panel\.body-scroll\s*\{[^}]*display:\s*flex/);
	});

	it('leaves every other menu scrolling the panel', () => {
		// The opt-in is the compatibility story: this primitive backs many menus
		// and none of them was asked to change.
		expect(menuSrc).toMatch(/\.menu-panel\s*\{[^}]*max-height:\s*340px/);
		expect(menuSrc).toMatch(/\.menu-panel\s*\{[^}]*overflow-y:\s*auto/);
		expect(menuSrc).toContain('bodyScroll = false');
	});
});
