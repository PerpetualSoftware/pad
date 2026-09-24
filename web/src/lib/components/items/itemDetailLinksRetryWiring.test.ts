// Node-project test (no DOM): a SOURCE-level guard that ItemDetail actually
// wires BUG-2992's links retry. `linksRetry.test.ts` vouches for the schedule;
// this vouches for the binding (CONVE-19: a direct-call test says nothing about
// whether the component calls it). Source text rather than a render for the
// reason `itemDetailLinksSurviveFailedRefresh.test.ts` gives: ItemDetail is too
// large to mount for a structural property, and comments are stripped first
// so the assertions are about code rather than prose that names the calls.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const raw = readFileSync(fileURLToPath(new URL('./ItemDetail.svelte', import.meta.url)), 'utf8');
const code = raw
	.replace(/<!--[\s\S]*?-->/g, '')
	.replace(/\/\*[\s\S]*?\*\//g, '')
	.replace(/(^|[^:])\/\/[^\n]*/g, '$1');

/** The `{...}` block starting at or after `from`, matched by balancing braces. */
function balancedBlock(source: string, from: number): string {
	const open = source.indexOf('{', from);
	if (open < 0) return '';
	let depth = 0;
	for (let i = open; i < source.length; i++) {
		if (source[i] === '{') depth++;
		else if (source[i] === '}' && --depth === 0) return source.slice(open, i + 1);
	}
	return '';
}

function blockAfter(marker: string): string {
	const at = code.indexOf(marker);
	expect(at, `precondition: "${marker}" is in ItemDetail's code`).toBeGreaterThan(-1);
	return balancedBlock(code, at);
}

describe('ItemDetail wires the links retry (BUG-2992)', () => {
	it('the preserving helper records a failure in its catch and a success in its try', () => {
		const helper = blockAfter('async function refreshLinksPreservingOnFailure');
		const catchAt = helper.indexOf('catch');
		expect(helper.slice(0, catchAt)).toContain('linksRetry.succeeded(');
		expect(balancedBlock(helper, catchAt)).toContain('linksRetry.failed(');
	});

	it('the initial load records a failure and a success too', () => {
		// The load's links try/catch is the one holding `linksHeldForItemId`.
		const at = code.indexOf('await api.links.list(wsSlug, itemData.slug)');
		expect(at, 'precondition: the load-site fetch').toBeGreaterThan(-1);
		const tail = code.slice(at, code.indexOf('linksHeldForItemId !== itemData.id', at));
		expect(tail).toContain('linksRetry.succeeded(itemData.id)');
		expect(tail).toContain('linksRetry.failed(');
	});

	it('a sync result kicks the retry BEFORE the caught_up return', () => {
		// A gap holding only link changes reports caught_up, because link
		// changes do not touch the item: kicking after that return would miss
		// exactly the case the retry exists for.
		const onSync = blockAfter('syncService.onSync(');
		const kick = onSync.indexOf('linksRetry.kick()');
		const caughtUp = onSync.indexOf("result.type === 'caught_up'");
		expect(kick).toBeGreaterThan(-1);
		expect(caughtUp).toBeGreaterThan(-1);
		expect(kick).toBeLessThan(caughtUp);
	});

	it('the retry is cancelled on destroy', () => {
		expect(blockAfter('onDestroy(() =>')).toContain('linksRetry.cancel()');
	});

	it('the attempt yields to a newer write as NOT fresh, and commits only after it', () => {
		// "Superseded" must answer false, or a re-read that bumped itemGen and
		// then failed on the item GET would clear the debt with nothing left to
		// correct it. The commit must follow the check, or the check guards
		// nothing.
		const body = blockAfter('async function retryLinks');
		const check = body.indexOf('if (myItemGen !== itemGen || itemLinks !== linksAtIssue) return false;');
		const commit = body.indexOf('itemLinks = links;');
		expect(check, 'the newer-write check answers false').toBeGreaterThan(-1);
		expect(commit).toBeGreaterThan(check);
	});

	it('the schedule runs retryLinks', () => {
		expect(code).toMatch(/const linksRetry = createLinksRetry\(\(target\) => retryLinks\(target\)\);/);
	});
});
