// Node-project SOURCE guard: every async commit point on the collection page
// is fenced on the signed-in identity (BUG-3084, surface 1 of 7).
//
// WHY SOURCE, and why it is not the only instrument: `collectionIdentityFence\
// .svelte.test.ts` next to this file mounts the page, holds a request open,
// moves the identity and asserts no commit lands — that owns the SEMANTICS of
// the handlers it drives. What it cannot do is tell you a TWENTY-SECOND commit
// point was added unfenced. The population is the property here, and a
// behavioural suite only ever covers the members someone wrote a case for.
// This guard owns the population; that one owns the semantics.
//
// WHY THE PAGE NEEDS A FENCE AT ALL, since `routes/+layout.svelte` reloads the
// tab on an identity change: that reload is CONDITIONAL, and this page sits in
// every window it leaves open.
//
//   - `'' -> user` (sign-IN from anonymous): `if (!previousUserId) return;` —
//     no reload and no clear at all, so an in-flight handler here runs to
//     completion and commits under the new identity.
//   - `user -> ''` (sign-out): clears persistent state but does NOT reload (a
//     reload aborts the sign-out sites' own navigation).
//   - `user -> user` (swap): reloads, leaving the pre-reload window the
//     workspace layout's own SSE comment already names.
//
// `auth.svelte.ts`'s comment about a `{#key authStore.identityEpoch}` remount
// of route components describes a design BUG-3005 replaced with that reload;
// no such key block exists in the tree. Do not restore a fence's justification
// from it.
//
// NOT THE SAME QUESTION as this page's existing guards. `loadSeq`,
// `collectionGen`, `renameNav` and the `ws !== wsSlug` snapshots ask WHICH
// ROUTE an answer describes; `localIndex.upsert`'s three refusals ask which
// PROJECTION SCOPE it belongs to, and the store's own comment says all three
// are session-local and bump only on this tab's resync. Neither moves when the
// signed-in user changes. `authStore.identityFence`'s doc comment draws the
// same line from the other end.
//
// WHAT A SOURCE GUARD CANNOT DO: it checks spellings. A fence comparing the
// wrong two values, one made unreachable by an earlier return, or one placed
// after the commit it guards all pass here. That is the behavioural suite's
// job, and the division is deliberate.
import { describe, it, expect } from 'vitest';
import { readFenceSource } from '../../../../test/identityFenceSource';

const src = readFenceSource(new URL('./+page.svelte', import.meta.url));
const CODE = src.code;

/**
 * Handlers that legitimately need no ENTRY CAPTURE, each with its reason. A
 * handler lands here only when capturing at its own entry would be WRONG — not
 * when a fence merely looks unnecessary.
 */
const NO_ENTRY_CAPTURE: Record<string, string> = {
	loadCollection:
		'the page load, and the site that RE-STAMPS identityEpochAtLoad. It is the ' +
		'writer of the value every other reader is forbidden to read.',
	runBulkOn:
		're-entered by its own Undo callback minutes later and possibly from another page, ' +
		'so its capture is taken by the CALLER at issue time and handed in; capturing at ' +
		'entry would capture the clicker and compare it with itself.',
};

describe('the collection page fences every async commit point', () => {
	it('declares the page-level fence helpers, re-stamped at load', () => {
		expect(CODE).toMatch(/let\s+identityEpochAtLoad\s*=\s*authStore\.identityEpoch/);
		expect(CODE).toMatch(
			/function captureIdentity\(\)\s*:\s*number\s*\{[\s\S]*?return authStore\.identityEpoch/
		);
		expect(CODE).toMatch(
			/function identityHeld\(captured: number\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === captured/
		);
		expect(CODE).toMatch(
			/function pageIdentityHeld\(\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === identityEpochAtLoad/
		);

		// RE-STAMPED before load()'s first await, or a page that survives an
		// identity change without remounting — which, per the header, is every
		// sign-in from anonymous — compares against a dead epoch for the rest
		// of its life.
		const load = src.asyncFunctions().get('loadCollection');
		expect(load, 'loadCollection() was renamed — re-point this guard').toBeDefined();
		const beforeFirstAwait = load!.slice(0, load!.indexOf('await'));
		expect(beforeFirstAwait).toContain('identityEpochAtLoad = authStore.identityEpoch');
	});

	it('enumerates the population it claims to cover', () => {
		// COUNTS ARE ASSERTED so a new member cannot arrive unnoticed: an added
		// handler, arrow, callback or timer fails HERE and has to be looked at
		// and dispositioned. That is the whole point of enumerating rather than
		// pattern-matching, and it is how BUG-3084's own table was found to be
		// four rows short of this file (checkpoint 1 on that item: the filed
		// survey used a function-level grep, which cannot see a subscription
		// callback).
		expect(src.asyncFunctions().size, 'a top-level async function was added or removed').toBe(15);
		expect(src.markupAsyncArrows(), 'an inline async arrow appeared in the markup').toHaveLength(0);
		expect(
			src.nestedAsyncCallbacks(),
			'an async callback that is neither a top-level declaration nor a markup arrow'
		).toHaveLength(4);
		expect(src.deferredTimers(), 'a setTimeout/setInterval was added or removed').toHaveLength(3);
	});

	it('captures the identity at ENTRY in every async handler, before its first await', () => {
		// The CAPTURE is the half a "does the body mention a fence" check
		// misses: an occurrence anywhere in the body satisfies that, including
		// one AFTER the write it was supposed to guard (#1370, codex round 1).
		const missing: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (name in NO_ENTRY_CAPTURE) continue;
			if (!body.includes('await')) continue;
			const beforeFirstAwait = body.slice(0, body.indexOf('await'));
			if (!beforeFirstAwait.includes('captureIdentity()')) missing.push(name);
		}
		expect(
			missing,
			'these async handlers do not capture the identity before their first await. A fence ' +
				'comparing against anything else can be defeated by a concurrent load re-stamping it.'
		).toEqual([]);
	});

	it('never lets a handler compare against the PAGE load epoch', () => {
		// `identityEpochAtLoad` is RE-STAMPED by loadCollection, so a handler
		// comparing against it can be defeated by a concurrent load setting it
		// to the very value the handler is about to see (#1370, codex round 1
		// [High], carried into this enumeration). Exactly one function may read
		// it: `pageIdentityHeld`, whose question is about the PAGE.
		const readers: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (name === 'loadCollection') continue; // the re-stamper, i.e. the writer
			if (body.includes('identityEpochAtLoad')) readers.push(name);
		}
		expect(
			readers,
			'these handlers read the page load epoch directly. It is re-stamped by loadCollection(), ' +
				"so a handler comparing against it commits when a concurrent load moves it to the " +
				"current value. Capture at the handler's own entry instead."
		).toEqual([]);
		expect(CODE).toMatch(/function pageIdentityHeld\(\)[\s\S]*?identityEpochAtLoad/);
	});

	it('checks the fence at least once per await in every async handler', () => {
		// COUNTED, not merely present. One check in a handler with four awaits
		// leaves three continuations unguarded, and "the body mentions a fence"
		// cannot tell those apart. A LOWER BOUND, not a proof — it cannot see
		// WHERE the checks sit, which is the behavioural suite's half.
		const short: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			const awaits = body.split('await ').length - 1;
			if (awaits === 0) continue;
			const checks =
				body.split('identityHeld(').length - 1 + body.split('pageIdentityHeld()').length - 1;
			if (checks < awaits) short.push(`${name} (${checks} checks for ${awaits} awaits)`);
		}
		expect(
			short,
			'these async handlers have fewer identity checks than awaits, so at least one continuation commits unguarded'
		).toEqual([]);
	});

	it('guards the SUCCESS continuation, not merely the failure one', () => {
		// The count above does not discriminate WHERE the checks are, and the
		// difference is reachable: `deleteView` has one await and two checks, so
		// deleting the one that guards its SUCCESS path still satisfies 1 >= 1.
		// That is mutant M7 of this unit's matrix, which SURVIVED the count.
		//
		// The narrow property that kills it: between a handler's FIRST await and
		// the end of its outer `try` — i.e. along the path a successful request
		// takes — there is at least one check. Narrow deliberately. A rule that
		// tried to balance every try/catch arm flagged `handleStatusChange`,
		// whose retry-await lives in a NESTED try inside the outer catch and
		// whose check sits after that nested block: correct code, called wrong
		// by an instrument that split text it could not parse. An instrument
		// that reports correct code is worse than one with a known blind spot,
		// because the blind spot can be written down and a false alarm gets
		// silenced.
		const unguarded: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			const firstAwait = body.indexOf('await ');
			if (firstAwait === -1) continue;
			const catchAt = body.indexOf('} catch');
			const successPath = body.slice(firstAwait, catchAt === -1 ? body.length : catchAt);
			if (!/identityHeld\(|pageIdentityHeld\(\)/.test(successPath)) {
				unguarded.push(name);
			}
		}
		expect(
			unguarded,
			'these handlers have no identity check between their first await and the end of their try block, ' +
				'so the path a SUCCESSFUL request takes commits unguarded even though the handler mentions a fence'
		).toEqual([]);
	});

	it('fences the async callbacks that are not declarations', () => {
		// The four here are an `$effect` IIFE, the SSE subscription, the sync
		// subscription and the debounced search timer. Two of them are the
		// `pageIdentityHeld` shape rather than the entry-capture one: a
		// subscription callback ARRIVES after the identity has already moved,
		// so a capture taken at its entry is the new value and can detect
		// nothing. Both questions are required of both, which is why this
		// asserts a fence is present rather than which one.
		for (const block of src.nestedAsyncCallbacks()) {
			expect(
				block.body,
				`an async callback commits without an identity fence: ${block.body.slice(0, 120)}`
			).toMatch(/identityHeld\(|pageIdentityHeld\(\)/);
		}
	});

	it('fences the deferred timers, which no await-adjacent check covers', () => {
		// A timer body runs after its await returned, so a check at the await
		// says nothing about who is signed in when it fires. These are
		// invisible to any instrument that enumerates awaits — and one of the
		// three (the debounced search) is ALSO a nested async callback above.
		// Overlapping populations, each asserted for its own property.
		//
		// The zero-delay `bypassNavGuard` timer is on the list too rather than
		// exempted by argument: `setTimeout(…, 0)` still yields to the task
		// queue, and "too short to matter" is a claim about a race, which is
		// the kind of claim this room does not accept without a measurement.
		for (const block of src.deferredTimers()) {
			expect(
				block.body,
				`a deferred timer commits without an identity fence: ${block.body.slice(0, 100)}`
			).toMatch(/identityHeld\(|pageIdentityHeld\(\)/);
		}
	});

	it('never ends a block with a fence, which guards nothing', () => {
		// A check placed AFTER the last commit in its block is dead code that
		// still satisfies every count above. This is not a hypothetical shape:
		// the first version of this unit put one at the end of the bootstrap
		// IIFE, where it followed the `deltaSync` it was meant to guard, and
		// every assertion in this file passed. Codex round 1 returned CLEAN on
		// it too.
		//
		// A trailing `if (!identityHeld(x)) return;` is the only spelling worth
		// forbidding — an early-return fence is the shape this codebase uses,
		// and a trailing one is always vacuous.
		const trailing = /if \(!(?:identityHeld\([^)]*\)|pageIdentityHeld\(\))\) return;\s*\}\s*$/;
		const offenders: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (trailing.test(body)) offenders.push(name);
		}
		for (const block of src.nestedAsyncCallbacks()) {
			if (trailing.test(block.body)) offenders.push(block.label);
		}
		for (const block of src.deferredTimers()) {
			if (trailing.test(block.body)) offenders.push(block.label);
		}
		expect(
			offenders,
			'these blocks END with an identity check, so it follows every commit it was meant to guard ' +
				'and stops nothing while still satisfying the counts above'
		).toEqual([]);
	});

	it('gives the bulk Undo toast its OWN captured epoch rather than the page helper', () => {
		// `runBulkOn` attaches an Undo button whose `onAction` is handed to the
		// GLOBAL toast store and deliberately outlives this page — the archive
		// and move flows re-enter `runBulkOn` from it. So the page helpers are
		// wrong for it twice over: the variable they read may belong to a dead
		// page, and a later load re-stamps it. It captures at ISSUE time and
		// compares at CLICK time, which is #1370's delete-Undo shape.
		const bulk = src.asyncFunctions().get('runBulkOn');
		expect(bulk, 'runBulkOn was renamed — re-point this guard').toBeDefined();
		expect(bulk!).toMatch(/onAction: \(\) => \{[\s\S]*?if \(!identityHeld\(/);
	});
});
