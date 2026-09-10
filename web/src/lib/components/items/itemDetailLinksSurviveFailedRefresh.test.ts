// Node-project test (no DOM): a SOURCE-level guard that a failed SAME-ITEM
// links refresh in ItemDetail keeps the links already on screen, instead of
// replacing them with an empty array (BUG-2871, the `:194` half).
//
// Why this matters beyond the lost data: an empty list collapses
// `{#if relationshipGroups.length > 0}`, which sits ABOVE both `{#each}` keys
// (`(group.label)` and `(entry.key)`) — so keying the rows protects nothing,
// and the whole relationships section is destroyed and later rebuilt. A click
// straddling that is swallowed entirely, because a click needs mousedown and
// mouseup on ONE node. That is the same defect BUG-2871 fixed for the Children
// pane, at the same altitude, through a different door.
//
// Why source text and not a render: ItemDetail is ~7,900 lines with collab,
// SSE and pane wiring, and the property is structural — the regression this
// guards is a future edit reintroducing `.catch(() => [])` at a refresh site,
// which shows up in the source and would not show up in any component suite.
// Same reasoning, and same file, as `itemDetailUsesPicker.test.ts`.
//
// WHAT A SOURCE GUARD CANNOT DO, stated so it is not mistaken for proof: it
// checks spellings, not behaviour. Review defeated three successive versions of
// these assertions with mutants written specifically against them — a helper
// returning [], one clearing before returning the field, an unconditional clear
// beside the gated one, and `itemLinks.length = 0` / `.splice(0)` in place of an
// assignment. Each is now refused, and the windows are brace-matched rather than
// fixed-length so a clear cannot sit just past the end of one. That is where the
// tightening stops: the remaining escapes require someone deliberately writing
// around a test in the file they are editing, and the honest instrument for
// behaviour is the E2E leg plus the reasoning on the BUG-2871 trail.
//
// COMMENTS ARE STRIPPED BEFORE ASSERTING, and that is load-bearing rather than
// tidiness: the helper's own doc comment quotes `.catch(() => [])` as the thing
// it replaced, and it names `api.links.list`. Asserting against raw source
// would match this file's own prose and the guard would pass or fail on
// documentation rather than on code — the same trap `itemDetailUsesPicker`
// records for HTML comments and `<ItemPicker`.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const raw = readFileSync(
	fileURLToPath(new URL('./ItemDetail.svelte', import.meta.url)),
	'utf8'
);

/**
 * Source with HTML comments, JS block comments and JS line comments removed.
 *
 * The line-comment rule skips `//` preceded by `:` so `https://` inside a
 * string survives as code rather than eating the rest of its line — a URL is
 * the one place `//` appears without starting a comment in this file.
 */
const code = raw
	.replace(/<!--[\s\S]*?-->/g, '')
	.replace(/\/\*[\s\S]*?\*\//g, '')
	.replace(/(^|[^:])\/\/[^\n]*/g, '$1');

const count = (haystack: string, needle: string) => haystack.split(needle).length - 1;

/**
 * The `{...}` block that starts at or after `from`, matched by BALANCING braces.
 *
 * The first version of this file sliced fixed-length windows (500 and 700
 * characters). Both were escapable — a clear placed past the window passes —
 * and the load-site window bled past its `catch` into unrelated member/role
 * loading, so an ordinary edit there could fail the count for a reason that has
 * nothing to do with this property (codex round 3). A balanced block is bounded
 * by the code's own structure instead of by a guess.
 *
 * Brace-counting is not a JS lexer: a `{` inside a string or a regex literal in
 * the region would skew it. Accepted here because the regions this is pointed at
 * are small and contain neither, and the test asserting a KNOWN count would fail
 * loudly rather than silently if that stopped being true.
 */
function balancedBlock(source: string, from: number): string {
	const open = source.indexOf('{', from);
	if (open < 0) return '';
	let depth = 0;
	for (let i = open; i < source.length; i++) {
		if (source[i] === '{') depth++;
		else if (source[i] === '}') {
			depth--;
			if (depth === 0) return source.slice(open, i + 1);
		}
	}
	return source.slice(open);
}

/**
 * Every way this file has of emptying `itemLinks` in place or by assignment.
 *
 * Assignment alone is not enough: `itemLinks.length = 0` and
 * `itemLinks.splice(0)` clear the visible links while passing an
 * assignment-only check (codex round 3 wrote both). A source guard cannot
 * enumerate every possible spelling — what it can do is refuse the ones a
 * reader would actually reach for.
 */
const MUTATES_ITEM_LINKS = /itemLinks\s*(?:=[^=]|\.length\s*=|\.splice\(|\.pop\(|\.shift\(|\.fill\()/;

describe('ItemDetail: a failed same-item links refresh keeps the links it has', () => {
	it('strips comments effectively enough for the assertions below to be about code', () => {
		// A control on the instrument itself (CONVE-30): if the strip silently
		// did nothing, every assertion here would still "pass" while measuring
		// prose rather than code.
		//
		// Anchored on a LONG-STANDING comment rather than on the helper's own
		// doc comment. Anchoring it on the new comment made the control fail
		// against pre-fix source — where the comment does not exist yet — so it
		// reported "the strip is broken" when the strip was fine and only the
		// fix was absent. A control that fails for the wrong reason is not a
		// control.
		expect(raw).toContain('isOwner now comes from workspaceStore');
		expect(code).not.toContain('isOwner now comes from workspaceStore');
	});

	it('has no refresh site that swallows a failure into an empty array', () => {
		// The exact shape that was there: `api.links.list(...).catch(() => [])`.
		// Matched loosely on purpose — any `.catch` that yields `[]` on this call
		// is the defect, however the arguments are spelled.
		expect(code).not.toMatch(/api\.links\.list\([^)]*\)\s*\.catch\(\s*\(\s*\)\s*=>\s*\[\s*\]\s*\)/);
	});

	it('routes every same-item refresh through the preserving helper', () => {
		// Three refresh callers: the SSE adopt path, the incremental adopt path,
		// and the long-absence reload. All three fetch by `updated.slug`, i.e.
		// the item already loaded, which is what makes preserving correct.
		const calls = count(code, 'await refreshLinksPreservingOnFailure(reqWsSlug, updated.slug)');
		expect(calls).toBe(3);
	});

	it('leaves exactly two direct api.links.list calls: the helper and the initial load', () => {
		// If a fourth appears, a new caller has been added that does not go
		// through the helper — which is how the three original sites drifted
		// into having the same bug three times.
		expect(count(code, 'api.links.list(')).toBe(2);
	});

	it('has a helper whose failure branch returns the CURRENT links, not an empty array', () => {
		// The gap codex found in the first version of this test: every other
		// assertion here passes against a helper spelled
		//
		//     catch { return []; }
		//
		// which reintroduces the exact defect while routing through the helper.
		// Rejecting the old spelling is not the same as asserting the new
		// behaviour, so this pins the fallback VALUE.
		expect(code).toMatch(
			/async function refreshLinksPreservingOnFailure\([^)]*\)[^{]*\{[\s\S]{0,400}?catch\s*\{[^}]*return itemLinks;[\s\S]{0,40}?\}/
		);
		// And the helper MUTATES NOTHING. Pinning only the return value admits
		// `catch { itemLinks = []; return itemLinks; }`, which satisfies every
		// other assertion here while reintroducing the defect (codex round 2
		// wrote that mutant). A helper that reads state and writes none is the
		// property; anything emptying `itemLinks` inside it is the defect
		// whatever it then returns.
		const helperStart = code.indexOf('async function refreshLinksPreservingOnFailure');
		expect(helperStart).toBeGreaterThan(-1);
		const helperBody = balancedBlock(code, helperStart);
		expect(helperBody).not.toMatch(/return\s*\[\s*\]/);
		expect(helperBody).not.toMatch(MUTATES_ITEM_LINKS);
	});

	it('clears links on a failed load only when they belong to a DIFFERENT item', () => {
		// `loadData` is not only a first load or a switch — the edit-collection
		// handler calls it for a SAME-item reload after a schema change, and an
		// unconditional clear there destroyed good rows (codex P1). The gate is
		// item identity captured before this load can replace `item`, not which
		// function is running.
		expect(code).toContain('const linksHeldForItemId = untrack(() => item?.id ?? null);');
		expect(code).toMatch(/if \(linksHeldForItemId !== itemData\.id\) itemLinks = \[\];/);

		// And that catch block empties `itemLinks` EXACTLY ONCE, gated. Pinning
		// only the presence of the gated line admits an unconditional clear
		// sitting beside it, or an `else` branch that clears anyway — both of
		// which restore the defect while passing the assertion above (codex
		// round 2). Counting is what makes the gate the only writer, and the
		// region is the catch block matched by BALANCED BRACES rather than a
		// fixed-length window, so a clear cannot sit just past the end of it and
		// an unrelated edit after the block cannot fail the count (round 3).
		const loadStart = code.indexOf('const links = await api.links.list(wsSlug, itemData.slug);');
		expect(loadStart).toBeGreaterThan(-1);
		const catchAt = code.indexOf('catch', loadStart);
		expect(catchAt).toBeGreaterThan(loadStart);
		const loadCatch = balancedBlock(code, catchAt);
		expect(loadCatch).toContain('linksHeldForItemId !== itemData.id');
		// One gated clear, and nothing else in the block touches the list.
		expect(count(loadCatch, 'itemLinks')).toBe(1);
	});

	it('still CLEARS links when the load is a real item SWITCH', () => {
		// The clear must still EXIST at this site, gated. It is the half most
		// likely to be "tidied" away by a reader who has just understood that
		// refreshes preserve: on a switch to a different item, keeping the
		// previous list shows one item's relationships under another's title.
		// The sibling test above pins the GATE; this one pins that there is
		// still something to gate.
		expect(code).toMatch(
			/const links = await api\.links\.list\(wsSlug, itemData\.slug\);[\s\S]{0,600}?catch\s*\{[\s\S]{0,400}?itemLinks = \[\];/
		);
	});

	it('reads the held item id through untrack, because loadData runs inside an $effect that writes item', () => {
		// CONVE-1688, and this one is not hypothetical: the first version of
		// this line was a plain `item?.id` read. `loadData` is called from an
		// `$effect` tracking wsSlug/collSlug/itemSlug, and it WRITES `item`
		// further down — so the plain read made that effect self-invalidating.
		// Dev throws `effect_update_depth_exceeded`; the PRODUCTION build
		// silently wedges the global effect scheduler, and the whole e2e suite
		// failed with nothing rendering, the attachment viewer included.
		//
		// Pinned here rather than left to review because the failure mode is
		// invisible in every cheaper gate: svelte-check, vitest and the unit
		// suites all passed while the built app rendered nothing.
		expect(code).toContain('const linksHeldForItemId = untrack(() => item?.id ?? null);');
		expect(code).not.toMatch(/const linksHeldForItemId = item\?\.id/);
	});

	it('keys the relationship rows, so the {#if} above them is the only node-identity gap', () => {
		// Recorded as an assertion rather than a comment because the whole
		// argument for this fix rests on it: the rows themselves ARE keyed, so
		// a re-derive over the same links preserves them, and the section-level
		// `{#if}` is what destroys nodes. If someone unkeys these, the fix stops
		// being sufficient and this test should be the thing that says so.
		expect(code).toContain('{#each relationshipGroups as group (group.label)}');
		expect(code).toContain('{#each group.entries as entry (entry.key)}');
		expect(code).toContain('{#if relationshipGroups.length > 0}');
	});
});
