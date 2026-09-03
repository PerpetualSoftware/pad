// Node-project test (no DOM): a SOURCE-level guard that ItemDetail actually
// CONSUMES ItemPicker rather than keeping a second copy of the search beside
// it (PLAN-2857 U3 / TASK-2862, "the extraction is proven by its first caller,
// not by a parallel copy").
//
// Why source text and not a render: ItemDetail is ~7,900 lines with collab,
// SSE and pane wiring, so mounting it to observe which search box it uses
// costs more setup than the property is worth — and the property is
// structural. The failure this guards against is a future edit re-inlining a
// search here, which shows up in the source and would NOT show up in
// ItemPicker's own suite.
//
// Identifier matches are END-anchored (`\b`): `searchItemsForLink` is a
// substring of `searchItemsForLinkV2`, and a guard that its own successor
// passes guards nothing.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const source = readFileSync(
	fileURLToPath(new URL('./ItemDetail.svelte', import.meta.url)),
	'utf8'
);

/**
 * Markup with HTML comments stripped. A mount assertion has to run against
 * this, not the raw text: `<!-- <ItemPicker ... -->` still matches
 * `/<ItemPicker\b/`, so a guard on the raw source passes against the exact
 * regression it exists to catch (verified — that mutant survived until this
 * was added). Comments are stripped rather than the regex made cleverer
 * because the same hazard applies to every match below.
 */
const markupSource = source.replace(/<!--[\s\S]*?-->/g, '');

describe('ItemDetail consumes ItemPicker', () => {
	it('imports it', () => {
		expect(source).toMatch(/import\s+ItemPicker\s+from\s+'\.\/ItemPicker\.svelte'/);
	});

	it('mounts it in the add-relationship form', () => {
		expect(markupSource).toMatch(/<ItemPicker\b/);
	});

	it('passes it the workspace and the exclusion set, and handles its selection', () => {
		const tag = markupSource.match(/<ItemPicker\b[\s\S]*?\/>/);
		expect(tag).not.toBeNull();
		const markup = tag![0];
		expect(markup).toMatch(/\{wsSlug\}/);
		expect(markup).toMatch(/excludeIds=\{addLinkExcludeIds\}/);
		expect(markup).toMatch(/onselect=\{handleCreateLink\}/);
	});

	it('no longer carries its own copy of the search', () => {
		for (const identifier of [
			'searchItemsForLink',
			'onAddLinkInput',
			'cancelAddLinkSearch',
			'addLinkResults',
			'addLinkLoading',
			'addLinkSearchSeq',
			'addLinkDebounceTimer',
			'ADD_LINK_SEARCH_DEBOUNCE_MS',
		]) {
			expect(source, `${identifier} should have moved into ItemPicker`).not.toMatch(
				new RegExp(`\\b${identifier}\\b`)
			);
		}
	});

	it('keeps the item-switch fence structural — the picker mounts inside {#key itemSlug}', () => {
		// PLAN-2105 / TASK-2112. The picker carries no second generation counter;
		// it relies on being DESTROYED on an item switch. There is more than one
		// `{#key itemSlug}` block in this file, so "a key exists before the
		// picker" would pass even if the picker sat outside all of them — the
		// nearest preceding key must also still be OPEN at the picker, i.e. no
		// `{/key}` in between.
		const picker = markupSource.indexOf('<ItemPicker');
		expect(picker).toBeGreaterThan(-1);

		const before = markupSource.slice(0, picker);
		const keyStart = before.lastIndexOf('{#key itemSlug}');
		expect(keyStart, 'no {#key itemSlug} precedes the picker').toBeGreaterThan(-1);
		expect(
			before.slice(keyStart).includes('{/key}'),
			'the nearest {#key itemSlug} closes before the picker — the picker is outside it'
		).toBe(false);
	});
});
