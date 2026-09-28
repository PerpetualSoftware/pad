import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// BUG-3231: six menus closed on a window/document CLICK whose target was not
// inside them, so a drag begun inside and released outside closed them. Each
// now dismisses through `clickOutside`, which decides on the press. This pins
// the population the census found (BUG-3229 trail): each file uses the action
// and no longer carries a click-based outside closer. A new window click
// closer elsewhere is not caught here; the census instruments on the trail are
// how to look for one.

const SRC = resolve(__dirname, '..', '..');
const POPULATION = [
	'lib/components/fields/FieldEditor.svelte',
	'lib/components/collections/BoardView.svelte',
	'lib/components/common/EmojiPickerButton.svelte',
	'lib/components/timeline/ReactionPicker.svelte',
	'routes/console/+layout.svelte',
];

// A RETIRED member. TopBar's member was the workspace overflow menu, deleted
// when the bar became a tab bar over the open set (TASK-3274); its user menu
// is on the shared Menu primitive. It no longer has a menu of its own to
// dismiss, so it is held to the half of the rule that still applies: no
// click-based outside closer comes back.
const RETIRED = ['lib/components/layout/TopBar.svelte'];

// A click listener on the window or document: the closer shape this replaced.
const CLICK_CLOSER = [
	/<svelte:window[^>]*\bonclick=/s,
	/<svelte:document[^>]*\bonclick=/s,
	/(?:window|document)\.addEventListener\(\s*['"]click['"]/,
];

describe('the click-outside menu population dismisses on the press (BUG-3231)', () => {
	for (const f of POPULATION) {
		it(`${f} uses clickOutside and has no window/document click closer`, () => {
			const source = readFileSync(resolve(SRC, f), 'utf8');
			expect(source).toMatch(/use:clickOutside=\{/);
			for (const re of CLICK_CLOSER) expect(source).not.toMatch(re);
		});
	}
	for (const f of RETIRED) {
		it(`${f} (retired) has no window/document click closer`, () => {
			const source = readFileSync(resolve(SRC, f), 'utf8');
			for (const re of CLICK_CLOSER) expect(source).not.toMatch(re);
		});
	}
});
