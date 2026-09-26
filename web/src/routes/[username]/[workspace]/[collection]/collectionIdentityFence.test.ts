// Node-project SOURCE guard: every async commit point on the collection page
// is fenced on the signed-in identity (BUG-3084, surface 1 of 7).
//
// THE GATE BESIDE THIS (TASK-3097): `collectionIdentityGate.test.ts` tables every async unit on
// this page with the hash of the code it was reviewed on, and refuses any
// edit to one until its row is re-read. That catches BUG-3084's round-4
// classes, which a source scanner cannot see. This file catches what the
// gate does not: the RULE on a NEW handler, which the gate accepts with any
// row. Keep both. Neither is a duplicate of the other.
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
import { readFenceSource, withoutCatchArms, trackedEpochReadDetails } from '../../../../test/identityFenceSource';

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

	it('re-loads on an identity change, so pageIdentityHeld() can recover', () => {
		// WITHOUT THIS THE FENCE IS A ONE-WAY DOOR (BUG-3084 repair, codex P1).
		// `identityEpochAtLoad` is re-stamped only by the page load, and that
		// load is driven by a ROUTE-keyed effect. `routes/+layout.svelte` does
		// not reload the tab on an anonymous -> signed-in transition, so without
		// a subscription the page keeps a stale epoch and every
		// `pageIdentityHeld()` is false for ever — the subscription callbacks
		// this fence protects go silently inert and never recover.
		//
		// Asserted in the SOURCE guard rather than only behaviourally because
		// this is a property every surface in the family owes, and the guard is
		// what surfaces 3-7 will copy.
		expect(
			CODE,
			'the page does not subscribe to authStore.onIdentityChange, so a stale load epoch is never ' +
				'refreshed and pageIdentityHeld() refuses for ever after a sign-in'
		).toMatch(/authStore\.onIdentityChange\(/);
		// And the subscription must actually re-run the load — a listener that
		// merely clears state would leave the epoch stale, which is the defect
		// wearing a different shape.
		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		expect(
			sub.slice(0, sub.indexOf('});')),
			'the identity-change listener does not re-run loadCollection, so the load epoch stays stale'
		).toMatch(/loadCollection\(/);
		// Unsubscribed, or it outlives the page it re-loads.
		expect(CODE).toMatch(/onDestroy\(stopIdentityReload\)/);
	});

	it('fences the SYNCHRONOUS commit points the await-based rules cannot see', () => {
		// A population neither instrument reaches: a function with NO await that
		// still replays intent formed earlier. `leaveDiscard` runs when the
		// unsaved-draft dialog is answered and replays `pendingNav`, which was
		// created when the PREVIOUS user tried to leave the page — so after an
		// identity change it navigates the new user to the previous user's
		// destination.
		//
		// The item's body predicted this shape exists ("commit points with NO
		// await at all") and said it is found by reading. It was found by a
		// reviewer instead, which is the same thing arriving later. Named
		// explicitly rather than enumerated, because "every synchronous function
		// that might replay captured intent" is not a set a grep can produce —
		// and a rule that cannot enumerate its population should not pretend to.
		const discard = CODE.slice(CODE.indexOf('function leaveDiscard()'));
		expect(
			discard.slice(0, discard.indexOf('\n\t}')),
			'leaveDiscard replays pendingNav without checking the identity, so a dialog answered after ' +
				"a sign-in navigates the new user to the previous user's destination"
		).toMatch(/pageIdentityHeld\(\)/);
	});

	it('clears the previous identity\'s page state before re-loading', () => {
		// The re-load is a round-trip, and `loadCollection` leaves the current
		// collection, views and progress on screen for its whole duration. The
		// fences stop stale async COMMITS; they say nothing about state already
		// rendered, so without this the previous user's private collection stays
		// visible across a sign-out (codex [P1]).
		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const body = sub.slice(0, sub.indexOf('});'));
		expect(body, 'the identity-change listener re-loads without clearing what is on screen')
			.toMatch(/collection = null/);
		// And the clear must come BEFORE the load, or it is decoration.
		expect(body.indexOf('collection = null')).toBeLessThan(body.indexOf('loadCollection('));
	});

	it('no $effect depends on the identity epoch without saying so', () => {
		// THE POPULATION NO OTHER RULE REACHES, and the one this family found
		// last and hardest (BUG-3084 checkpoint 18).
		//
		// `authStore.identityEpoch` is `$state`. An `$effect` that reads it
		// SYNCHRONOUSLY takes a DEPENDENCY on it, so an identity change re-runs
		// that effect. On this page that re-ran the search dispatch, which
		// `clearTimeout`s its armed debounce and arms a new one — still reading
		// `searchQuery`, the PREVIOUS user's typed text — while capturing the
		// NEW epoch. The fence inside the timer then passed, because the epoch
		// it captured was current, and the old query went to the server under
		// the new identity. The guard did not fail; it was RE-CREATED by the
		// framework at the one moment it needed to stay put.
		//
		// SYNCHRONOUS is the whole predicate. A read inside a nested callback —
		// the SSE subscription's `pageIdentityHeld()`, for instance — happens
		// when the event arrives, outside the reactive scope, and creates no
		// dependency. A rule that greps the effect's text flags it anyway, and
		// the one site that matters then hides among false positives. Hence
		// `syncBodyOnly`.
		//
		// NOT a blanket ban, which is the correction this rule owes its first
		// reading: two of this family's three tracked reads WANT the dependency.
		// So each is dispositioned by name, and an undispositioned one fails.
		// KEYED ON A MARKER IN THE SYNCHRONOUS BODY, which is where the rule
		// looks — the first spelling keyed on `localIndex.bootstrap`, which sits
		// inside the effect's async IIFE and is therefore stripped before the
		// rule ever sees it. The marker below is also the honest one: this
		// effect reads `authStore.userId` deliberately, and that read alone
		// already makes it re-run on an identity change, so the epoch read adds
		// no dependency it did not already have.
		// DISPOSITIONED PER EFFECT, and each entry names WHICH READS it covers.
		//
		// Exempting a whole effect was wrong and hid this unit's own defect: the
		// search entry skipped the synchronous capture along with the timer's
		// reads, so removing that capture's `untrack` produced ZERO offenders
		// (codex round 2 [P2]). An entry now lists the read tokens it vouches
		// for, and any other read in that effect is an offender.
		const INTENDED_DEPENDENCY: Record<string, { allowedReads: string[]; why: string; afterMarker?: string }> = {
			// Keyed on the shared helper's call since BUG-3181 moved the bootstrap
			// → identity check → reconcile sequence out of this page into
			// `enterWorkspaceIndex` (its order and sole-caller guard live in
			// lib/stores/workspaceIndexEntry.test.ts).
			'enterWorkspaceIndex(': {
				allowedReads: ['captureIdentity(', 'identityHeld('],
				why:
					'the bootstrap/reconcile effect re-runs on an identity change BY DESIGN — it reads ' +
					'authStore.userId synchronously and on purpose, so the capture is the CURRENT one ' +
					'and its fences stop the PREVIOUS run\'s continuation.',
			},
			'sseService.onItemEvent': {
				afterMarker: 'sseService.onItemEvent',
				allowedReads: ['pageIdentityHeld(', 'identityHeld(', 'captureIdentity('],
				why:
					'every epoch read in this effect is inside the SUBSCRIPTION CALLBACK, which runs ' +
					'when an event arrives — outside the reactive scope, creating no dependency. The ' +
					'broad allowance is safe only because the effect has NO epoch read before it ' +
					'subscribes, which the assertion below checks directly rather than assuming.',
			},
			'searchTimeout = setTimeout': {
				afterMarker: 'searchTimeout = setTimeout',
				allowedReads: ['identityHeld('],
				why:
					'the remaining reads are inside the 200ms TIMER callback, which runs long after ' +
					'the effect. The schedule-time CAPTURE is deliberately NOT allowed here — it ran ' +
					'synchronously and was this unit\'s defect, so it must stay untracked at the read ' +
					'rather than be exempted by its effect\'s disposition.',
			},
		};

		const offenders: string[] = [];
		for (const block of src.effectBlocks()) {
			const entry = Object.entries(INTENDED_DEPENDENCY).find(([k]) => block.body.includes(k));
			// PER READ, not per block: testing for `untrack(` anywhere in the
			// effect exempted `const e = captureIdentity(); untrack(() => x());`.
			// POSITIONAL as well as by token (codex round 3 [P2]). Without the
			// marker, an entry written for reads inside a timer covered a read
			// added SYNCHRONOUSLY beside the untracked capture — which puts the
			// dependency straight back while the rule reports nothing.
			const markerAt = entry?.[1].afterMarker ? block.body.indexOf(entry[1].afterMarker) : -1;
			for (const read of trackedEpochReadDetails(block.body)) {
				const positionOk = markerAt === -1 || read.index > markerAt;
				if (entry && positionOk && entry[1].allowedReads.some((t) => read.token.startsWith(t))) {
					continue;
				}
				offenders.push(`${block.label}: ...${read.context}`);
			}
		}
		expect(
			offenders,
			'these $effects read the identity epoch without untracking THAT READ and without their ' +
				'effect\'s disposition vouching for THAT read. Each therefore re-runs on an identity ' +
				'change, re-creating whatever it armed — under the new epoch, over the previous ' +
				'user\'s state.'
		).toEqual([]);

		expect(
			src.effectBlocks().length,
			'no $effect found — re-point this guard rather than reading its silence as compliance'
		).toBeGreaterThan(0);

		// The SSE entry allows every read token, which is only defensible while
		// the effect reads nothing before it subscribes. Checked, not assumed:
		// a read added to the synchronous prefix would otherwise inherit the
		// callback's exemption and re-run the subscription on every identity
		// change (codex round 2 [P2]).
		const sse = src.effectBlocks().find((b) => b.body.includes('sseService.onItemEvent'));
		expect(sse, 'the SSE effect was not found — re-point this guard').toBeDefined();
		const beforeSubscribe = sse!.body.slice(0, sse!.body.indexOf('sseService.onItemEvent'));
		expect(
			trackedEpochReadDetails(beforeSubscribe),
			'the SSE effect now reads the identity epoch BEFORE subscribing, so it takes a dependency ' +
				'and re-subscribes on every identity change — and its disposition, written for reads ' +
				'inside the callback, would silently cover it'
		).toEqual([]);
	});

	it('the identity-change listener clears every piece of per-session state', () => {
		// ENUMERATED against the file's own `$state` declarations (CONVE-35), so
		// a new piece of typed input or open dialog cannot arrive undispositioned.
		const reset = CODE.slice(CODE.indexOf('function resetPerSessionState()'));
		const resetBody = reset.slice(0, reset.indexOf('\n\t}'));
		expect(resetBody.length, 'resetPerSessionState() not found — re-point this guard').toBeGreaterThan(50);

		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const listener = sub.slice(0, sub.indexOf('\t});'));
		expect(
			listener,
			'the identity-change listener does not reset per-session state'
		).toMatch(/resetPerSessionState\(\)/);

		// Cleared by the listener directly (page DATA the reload replaces).
		const CLEARED_AS_DATA = [
			'collection', 'savedViews', 'activeViewId', 'itemProgress',
			'workspaceMembers', 'metaError', 'metaFromCache',
		];
		const NOT_PER_SESSION: Record<string, string> = {
			metaLoading: 'owned by loadCollection',
			progressLabel: 'replaced by the reload',
			boardColumns: 'derived from the item list the reload replaces',
			defaultViewId: 're-read from localStorage on render',
			// CORRECTED (codex round 1 [P3]): the previous justification said
			// "reset by the load it gates", and `loadCollection` never resets it
			// — only the route effect does. On a SAME-ROUTE identity reload the
			// flag therefore stayed true and the new user's default view was
			// never applied. It is reset in `resetPerSessionState()` now, so
			// this entry is gone rather than re-argued.

			urlFiltersLoaded: 'reset by the route change it gates',
			searchInputEl: 'DOM binding — Svelte clears it on unmount',
			saveViewInput: 'DOM binding',
			quickCreateInput: 'DOM binding',
			paneHostEl: 'DOM binding',
			listColumnEl: 'DOM binding (BUG-3165 list scroll handoff)',
			viewMenuTrigger: 'DOM binding',
			sortMenuTrigger: 'DOM binding',
			collMenuTrigger: 'DOM binding',
			paneMintRef: 'derived from the URL, which the identity change does not move',
			// URL-DERIVED, and deliberately kept: the address is the same page
			// for whoever is looking at it, so these are not the previous
			// SESSION's state. `unparentedFilter`'s own comment already says a
			// restricted caller's carried-over intent filters nothing.
			viewMode: 'URL/preference — belongs to the address, not the session',
			sortMode: 'URL/preference',
			showArchived: 'URL/preference',
			activeFilters: 'URL-derived',
			selectedTags: 'URL-derived',
			unparentedFilter: 'URL-derived, and gated against a restricted caller already',
		};

		// Enumerated by the core, by STATEMENT (BUG-3084 M12): the regex this
		// guard carried let a type annotation run across a newline into the
		// next statement, so an uninitialised typed `let` above a `$state` line
		// swallowed it. No such pair here today; the hole was real regardless.
		const declared = src.stateDeclarations();
		expect(declared.length, 'no $state declarations found — re-point this guard').toBeGreaterThan(40);

		const undispositioned = declared.filter(
			(n) =>
				!(n in NOT_PER_SESSION) &&
				!CLEARED_AS_DATA.includes(n) &&
				!new RegExp(`\\b${n}\\s*=`).test(resetBody)
		);
		expect(
			undispositioned,
			'these $state values are neither page data, nor reset on an identity change, nor named ' +
				'as something that legitimately survives one. Each may be the previous user\'s typed ' +
				'input or open dialog inherited by whoever signs in next: decide which, and either ' +
				'clear it or name it here.'
		).toEqual([]);

		// The two this unit exists for, asserted by name so a refactor that
		// renames the reset cannot quietly drop them.
		expect(resetBody, "the previous user's search text is inherited").toMatch(/searchQuery = ''/);
		expect(
			resetBody,
			"the previous identity's search RESULTS stay rendered — nothing else clears them once the " +
				'search effect no longer re-runs on an identity change'
		).toMatch(/searchResultRank = null/);
	});

	it('a stale continuation does not release the new identity\'s busy flags', () => {
		// The REVERSAL the roles board made in its round 4, applied here (codex
		// round 1 [P2]). These releases were unconditional on the grounds that a
		// busy flag left true disables its affordance for ever. That held while
		// nothing else cleared them; `resetPerSessionState()` now does, on every
		// identity change, synchronously and before any stale continuation can
		// resume. So clearing again from a previous-identity continuation can
		// only release a gate the NEW user's own in-flight operation holds,
		// letting them submit twice.
		//
		// Named per flag rather than derived, because "which check governs this
		// statement" is not a question a regex answers — the same reason the
		// library page names its two `finally` arms (mutant L6 there).
		const BUSY_FLAGS = ['creatingNew', 'savingView', 'savingDrafts'];
		const resetStart = CODE.indexOf('function resetPerSessionState()');
		expect(resetStart, 'resetPerSessionState() not found — re-point this guard').toBeGreaterThan(-1);
		const resetEnd = CODE.indexOf('\n\t}', resetStart);
		const unfenced: string[] = [];
		for (const flag of BUSY_FLAGS) {
			const re = new RegExp(`(.{0,70})\\b${flag} = false;`, 'g');
			let m: RegExpExecArray | null;
			while ((m = re.exec(CODE)) !== null) {
				// Inside `resetPerSessionState()` these are unconditional by
				// design — that function IS the identity change.
				if (m.index > resetStart && m.index < resetEnd) continue;
				if (/identityHeld\(/.test(m[1]!)) continue;
				unfenced.push(`${flag}: ...${m[1]!.replace(/\s+/g, ' ').trim().slice(-60)}`);
			}
		}
		expect(
			unfenced,
			'these busy-flag releases are unconditional, so a request issued by the PREVIOUS identity ' +
				"releases a gate the NEW user's own in-flight operation is holding"
		).toEqual([]);
	});

	it('enumerates the population it claims to cover', () => {
		// COUNTS ARE ASSERTED so a new member cannot arrive unnoticed: an added
		// handler, arrow, callback or timer fails HERE and has to be looked at
		// and dispositioned. That is the whole point of enumerating rather than
		// pattern-matching, and it is how BUG-3084's own table was found to be
		// four rows short of this file (checkpoint 1 on that item: the filed
		// survey used a function-level grep, which cannot see a subscription
		// callback).
		// 15 → 14 (BUG-3102): `handleRestore` was DELETED, not refactored. It had
		// no caller — no prop, no markup reference, no export — so it was an
		// unreachable async handler this guard had been faithfully counting and
		// fencing for as long as it existed. Single-item restore is owned by
		// ItemDetail. The guard doing exactly this on the deletion is it
		// working: the population is asserted so a member cannot leave
		// unnoticed any more than it can arrive unnoticed.
		expect(src.asyncFunctions().size, 'a top-level async function was added or removed').toBe(14);
		expect(src.markupAsyncArrows(), 'an inline async arrow appeared in the markup').toHaveLength(0);
		// 4 → 3 (BUG-3181): the bootstrap `$effect`'s async IIFE left this page.
		// Its bootstrap → identity check → reconcile sequence is now the shared
		// `enterWorkspaceIndex`, whose order and sole-caller status are pinned in
		// lib/stores/workspaceIndexEntry.test.ts; the effect keeps its
		// synchronous capture, which the INTENDED_DEPENDENCY entry above covers.
		expect(
			src.nestedAsyncCallbacks(),
			'an async callback that is neither a top-level declaration nor a markup arrow'
		).toHaveLength(3);
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
		// WINDOW WIDENED (BUG-3084 surface 2): the body with every CATCH ARM
		// EXCISED, not the text before the first `} catch`. The narrower form
		// shipped here first and reports correct code as unguarded whenever a
		// handler commits AFTER its try/catch rather than inside it — which the
		// roles board's `handleLaneDrop` does. Changed in both guards at once so
		// the family does not carry two answers to one question.
		const unguarded: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			const successOnly = withoutCatchArms(body);
			const firstAwait = successOnly.indexOf('await ');
			if (firstAwait === -1) continue;
			if (!/identityHeld\(|pageIdentityHeld\(\)/.test(successOnly.slice(firstAwait))) {
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
		// The three here are the SSE subscription, the sync subscription and the
		// debounced search timer (the bootstrap `$effect` IIFE that was a fourth
		// moved into `enterWorkspaceIndex` — BUG-3181). Two of them are the
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
