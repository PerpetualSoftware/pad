/**
 * BUG-3084 surface 4 — the SOURCE half. `dashboardIdentityFence.svelte.test.ts`
 * beside this file owns the SEMANTICS; this owns the POPULATION.
 *
 * The page is small — one async function, one timer — and the temptation on a
 * small surface is to assert less. The filed survey counted this page as ONE
 * commit point and the widened instrument found two (checkpoint 6); reading
 * found a third (the sync-subscription callback) that no instrument
 * enumerates. So the rules here are the family's in full, plus the three this
 * page's own shape needs:
 *   - every caller of `load()` is enumerated and held to writing nothing else,
 *     because the fence lives in `load()` and a caller that commits beside the
 *     call is a commit point the fence never sees;
 *   - the load effect calls `load()` INSIDE `untrack`, positionally, because
 *     the entry capture inside `load()` runs synchronously and would otherwise
 *     make the effect depend on the epoch — the core cannot see through the
 *     call, so this guard says it;
 *   - the identity listener resets and does NOT reload, because the keyed
 *     effect already does, and a second load per identity change is #1378's
 *     round-1 finding on a page that had the single load built in.
 *
 * Every rule asks a REFUSAL question. The behavioural suite owes the other
 * half — that the page keeps working after the identity moves — and the core's
 * header says why a surface that only ever asks "did it refuse?" cannot tell a
 * fence from an outage.
 */
import { describe, it, expect } from 'vitest';
import {
	readFenceSource,
	withoutCatchArms,
	trackedEpochReadDetails,
	untrackedSpans,
	matchBrace,
	matchDelimiter,
} from '../../../test/identityFenceSource';

const src = readFenceSource(new URL('./+page.svelte', import.meta.url));
const CODE = src.code;

/** `load()`'s body, or a failure that says the guard needs re-pointing. */
function loadBody(): string {
	const body = src.asyncFunctions().get('load');
	expect(body, 'load() was renamed — re-point this guard').toBeDefined();
	return body!;
}

/**
 * The `$effect` whose body dispatches `load(`: the keyed load effect. Found by
 * content rather than by position so a reordering of the effects does not
 * silently re-point every assertion below at a different block.
 */
function loadEffect(): string {
	const blocks = src.effectBlocks().filter((b) => /\bload\(/.test(b.body));
	expect(blocks.length, 'exactly one $effect should dispatch load() — the keyed load effect').toBe(1);
	return blocks[0]!.body;
}

/**
 * The body of the callback passed to `syncService.onSync(`. The core has no
 * enumerator for subscription callbacks — it names that as a limitation and
 * leaves the claim to the page's guard — so this delimits it directly and
 * FAILS CLOSED when it cannot.
 */
function syncCallbackBody(): string {
	const at = CODE.indexOf('syncService.onSync(');
	expect(at, 'the page no longer subscribes to syncService.onSync — re-point this guard').toBeGreaterThan(-1);
	const open = CODE.indexOf('{', at);
	const close = matchBrace(CODE, open);
	if (close === -1) throw new Error('could not delimit the onSync callback — re-point this guard');
	return CODE.slice(open, close + 1);
}

describe('the dashboard fences every async commit point', () => {
	it('declares the two page-level fence helpers, and deliberately not the page-load epoch', () => {
		expect(CODE).toMatch(
			/function captureIdentity\(\)\s*:\s*number\s*\{[\s\S]*?return authStore\.identityEpoch/
		);
		expect(CODE).toMatch(
			/function identityHeld\(captured: number\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === captured/
		);
		// A DECISION, recorded so it is re-made rather than drifted past: no
		// handler on this page composes a write from state the previous identity
		// loaded, so there is nothing for a page-level stamp to vouch for. The
		// moment one does, this assertion is the prompt to add the helper AND
		// the re-stamp rules the library guard carries — not to delete the line.
		expect(
			CODE,
			'the page now declares a page-load epoch. That is fine, but it comes with the family\'s ' +
				're-stamp rules (after the state it vouches for, on every path out of load) — bring ' +
				'them across from libraryIdentityFence.test.ts rather than only deleting this assertion'
		).not.toMatch(/identityEpochAtLoad|pageIdentityHeld/);
	});

	it('enumerates the population it claims to cover', () => {
		// COUNTS ASSERTED so a new member cannot arrive unnoticed. The filed
		// survey reported 1; the widened instrument reports 2 (checkpoint 6).
		expect(src.asyncFunctions().size, 'a top-level async function was added or removed').toBe(1);
		expect([...src.asyncFunctions().keys()]).toEqual(['load']);
		expect(src.markupAsyncArrows(), 'an inline async arrow appeared in the markup').toHaveLength(0);
		expect(src.nestedAsyncCallbacks(), 'an async callback arrived').toHaveLength(0);
		expect(src.deferredTimers(), 'a setTimeout/setInterval arrived or left').toHaveLength(1);
		expect(src.effectBlocks(), 'an $effect arrived or left — disposition it below').toHaveLength(7);
	});

	it('enumerates every caller of load(), which is where the fence lives', () => {
		// The one assertion this page needs that the family's shape does not
		// give it. The fence is inside `load()`; every path funnels into it. A
		// NEW caller that also writes state beside the call is a commit point
		// no other rule here can see, so the callers are counted and each is
		// named with what it may do.
		const calls = [...CODE.matchAll(/\bload\((wsSlug|slug)[^)]*\)/g)].map((m) => ({
			index: m.index ?? 0,
			text: m[0],
		}));
		// The declaration is `async function load(slug` and does not match the
		// argument shapes above; the recursive-looking `load(slug` inside the
		// body would, so exclude anything inside load's own body.
		const declAt = CODE.indexOf('async function load(');
		const declEnd = matchBrace(CODE, CODE.indexOf('{', declAt));
		const sites = calls.filter((c) => c.index < declAt || c.index > declEnd);
		expect(
			sites.map((s) => s.text),
			'the set of load() callers changed — add the new one to the list below with what it may ' +
				'write beside the call, or remove the one that left'
		).toEqual([
			'load(wsSlug)', // the keyed load effect
			'load(wsSlug, true)', // the 30 s poll
			'load(wsSlug, true)', // the sync-subscription callback
			'load(wsSlug)', // the Retry button
			'load(wsSlug, true)', // CreateCollectionModal oncreated
		]);
	});

	it('captures the identity at ENTRY, before the first await', () => {
		const body = loadBody();
		expect(body.slice(0, body.indexOf('await'))).toContain('const epochAtEntry = captureIdentity()');
	});

	it('checks the fence at least once per await', () => {
		const body = loadBody();
		const awaits = body.split('await ').length - 1;
		const checks = body.split('identityHeld(').length - 1;
		expect(awaits, 'load() no longer awaits — this guard measures nothing').toBeGreaterThan(0);
		expect(checks, `load() has ${checks} identity checks for ${awaits} awaits`).toBeGreaterThanOrEqual(awaits);
	});

	it('guards the SUCCESS continuation, not merely the failure one', () => {
		const successOnly = withoutCatchArms(loadBody());
		const firstAwait = successOnly.indexOf('await ');
		expect(firstAwait).toBeGreaterThan(-1);
		expect(
			successOnly.slice(firstAwait),
			'no identity check between the first await and the end of the try block, so the path a ' +
				'SUCCESSFUL request takes commits unguarded'
		).toMatch(/identityHeld\(/);
	});

	it('every board write in load() sits behind a check made SINCE the last await', () => {
		// The commit points, named. `loading` is deliberately absent — the
		// finally arm is seq-gated and NOT identity-gated, the library's
		// asymmetry (#1378): pinning the skeleton for ever is the worse failure.
		const COMMITS = ['dashboard', 'dashboardSlug', 'collections', 'dashError'];
		const body = loadBody();
		const offenders: string[] = [];
		for (const m of body.matchAll(new RegExp(`\\b(${COMMITS.join('|')})\\s*=(?!=)`, 'g'))) {
			const at = m.index ?? 0;
			const lastAwait = body.slice(0, at).lastIndexOf('await ');
			if (lastAwait === -1) {
				offenders.push(`${m[1]} written before any await — a synchronous commit under the ` +
					'current epoch is fine, but it is not what this page does; re-point this guard');
				continue;
			}
			if (!/identityHeld\(/.test(body.slice(lastAwait, at))) {
				offenders.push(`${m[1]} (no identity check since its await)`);
			}
		}
		expect(offenders, 'these writes commit with no identity check since the await before them').toEqual([]);
		expect(
			(body.match(new RegExp(`\\b(${COMMITS.join('|')})\\s*=(?!=)`, 'g')) ?? []).length,
			'no board writes found in load() — re-point this guard'
		).toBeGreaterThanOrEqual(6);
	});

	it('the catch arm refuses before it writes, on the same terms as the success arm', () => {
		// A Retry state belonging to the previous user is a commit. The library
		// rule "guard the success path" cannot see this arm — `withoutCatchArms`
		// excises it — so it gets its own assertion.
		const body = loadBody();
		const cat = body.indexOf('} catch (err) {');
		expect(cat, 'load() no longer has a catch arm — re-point this guard').toBeGreaterThan(-1);
		const armOpen = body.indexOf('{', cat + 1);
		const arm = body.slice(armOpen, matchBrace(body, armOpen) + 1);
		const firstWrite = arm.search(/\b(dashboard|dashboardSlug|dashError)\s*=(?!=)/);
		expect(firstWrite, 'the catch arm no longer writes — re-point this guard').toBeGreaterThan(-1);
		expect(
			arm.slice(0, firstWrite),
			'the catch arm writes the error state before checking the identity'
		).toMatch(/if \(seq !== dashLoadSeq \|\| !identityHeld\(epochAtEntry\)\) return;/);
	});

	it('the finally arm is seq-gated and NOT identity-gated, by decision', () => {
		const body = loadBody();
		const fin = body.indexOf('} finally {');
		expect(fin, 'load() no longer has a finally arm — re-point this guard').toBeGreaterThan(-1);
		const arm = body.slice(fin);
		expect(arm).toMatch(/if \(seq === dashLoadSeq\) loading = false;/);
		expect(
			arm,
			'the finally arm now gates `loading` on the identity. That pins the skeleton for ever on any ' +
				'path where the keyed reload does not come — read the asymmetry note in load() before ' +
				'changing this'
		).not.toMatch(/identityHeld\([^)]*\)[^;]*loading/);
	});

	it('a stale load may not write the page it no longer owns', () => {
		// Different question from the identity fence, and neither implies the
		// other: both loads can belong to the SAME user, which is exactly what
		// a workspace switch produces.
		const body = loadBody();
		expect(body).toMatch(/const seq = \+\+dashLoadSeq/);
		expect(
			(body.match(/seq !== dashLoadSeq/g) ?? []).length,
			'load() does not gate both its commit arms on the sequence token'
		).toBeGreaterThanOrEqual(2);
	});

	it('a lost identity issues no requests on behalf of the previous user', () => {
		// The check between the two awaits. `setCurrent` is awaited first; a
		// load that has lost its identity there must not go on to fetch the
		// board — the cookie is the new user's, the answer would be theirs, and
		// the continuation must discard it anyway.
		const body = loadBody();
		const first = body.indexOf('await workspaceStore.setCurrent(');
		const fetch = body.indexOf('api.dashboard.get(');
		expect(first).toBeGreaterThan(-1);
		expect(fetch).toBeGreaterThan(first);
		expect(body.slice(first, fetch)).toMatch(/if \(!identityHeld\(epochAtEntry\)\) return;/);
	});

	it('the deferred timer delegates to load() and writes nothing itself', () => {
		// The member the filed survey could not see (checkpoint 6). Its whole
		// job is to call `load()`, which carries the fence; anything else it
		// wrote would be a commit no rule above reaches.
		const [timer] = src.deferredTimers();
		expect(timer, 'the poll timer left — re-point this guard').toBeDefined();
		expect(timer!.body).toMatch(/\bload\(wsSlug, true\)/);
		expect(
			timer!.body,
			'the poll timer writes state beside its load() call — a commit point the fence in load() ' +
				'does not cover'
		).not.toMatch(/[^=!<>]=(?!=)/);
	});

	it('the sync-subscription callback delegates to load() and writes nothing itself', () => {
		// Enumerated by READING, because no instrument in the core enumerates
		// subscription callbacks — the core says so and leaves the claim here,
		// where it can be argued with.
		const body = syncCallbackBody();
		expect(body).toMatch(/\bload\(wsSlug, true\)/);
		expect(
			body,
			'the sync callback writes state beside its load() call — a commit point the fence in ' +
				'load() does not cover'
		).not.toMatch(/[^=!<>]=(?!=)/);
	});

	it('the keyed load effect calls load() only inside untrack, positionally', () => {
		// `load()` captures the identity SYNCHRONOUSLY before its first await.
		// Svelte tracks reads through ordinary calls, so an effect that called
		// it tracked would depend on the epoch and re-run on every identity
		// change — on top of the reload this same effect already performs for
		// that event through `sessionUserId`. The core cannot see through the
		// call; this rule says what it cannot.
		//
		// POSITIONAL: the call's index must fall inside an untracked span, not
		// merely share a block with one (the codex round-1 and round-3 defeats
		// on the collection page, both walked through a per-block check).
		const body = loadEffect();
		const spans = untrackedSpans(body);
		expect(spans.length, 'the load effect has no untrack — every load is issued twice').toBeGreaterThan(0);
		const at = body.search(/\bload\(wsSlug\)/);
		expect(at, 'the load effect no longer calls load(wsSlug) — re-point this guard').toBeGreaterThan(-1);
		expect(
			spans.some(([a, b]) => at >= a && at <= b),
			'load(wsSlug) is called OUTSIDE the untrack callback, so the effect takes a dependency on ' +
				'the epoch through the synchronous entry capture'
		).toBe(true);
	});

	it('the keyed load effect is the recovery: keyed on the user, and it clears before it loads', () => {
		// The family property, satisfied the settings way (#1370): clear FIRST,
		// then reload. The key must include the identity, or a sign-in on the
		// same route reloads nothing and the fence latches shut (#1372).
		const body = loadEffect();
		expect(body).toMatch(/\$\{sessionUserId\}\\n\$\{wsSlug\}/);
		expect(CODE).toMatch(/let sessionUserId = \$derived\(authStore\.userId\)/);
		const clear = body.indexOf('dashboard = null');
		const load = body.search(/\bload\(wsSlug\)/);
		expect(clear, 'the load effect no longer drops the board before reloading').toBeGreaterThan(-1);
		expect(clear, 'the board is dropped AFTER the reload is dispatched').toBeLessThan(load);
		for (const v of ['dashboardSlug = null', 'collections = []', 'dashError = null', 'onboardingTrack = null']) {
			expect(body.indexOf(v), `${v} is not cleared before the reload`).toBeGreaterThan(-1);
			expect(body.indexOf(v)).toBeLessThan(load);
		}
	});

	it('subscribes to identity changes, resets, does NOT reload, and unsubscribes', () => {
		expect(CODE).toMatch(/const stopIdentityWatch = authStore\.onIdentityChange\(/);
		expect(CODE).toMatch(/onDestroy\(stopIdentityWatch\)/);
		const at = CODE.indexOf('authStore.onIdentityChange(');
		const open = CODE.indexOf('(', at + 'authStore.onIdentityChange'.length);
		const close = matchDelimiter(CODE, open, '(', ')');
		if (close === -1) throw new Error('could not delimit the identity listener — re-point this guard');
		const listener = CODE.slice(open, close + 1);
		expect(listener).toMatch(/resetTransientState\(\)/);
		expect(
			listener,
			'the identity listener reloads. The keyed load effect already reloads for this event, so ' +
				'this is the library\'s double load (#1378 codex round 1) on a page that had the single ' +
				'one built in'
		).not.toMatch(/\bload\(/);
	});

	it('the identity listener resets every piece of transient interaction state', () => {
		// ENUMERATED rather than sampled (CONVE-35), held against the file's
		// own `$state` declarations so a new piece of interaction state cannot
		// arrive without being dispositioned.
		const reset = CODE.slice(CODE.indexOf('function resetTransientState()'));
		const resetBody = reset.slice(0, reset.indexOf('\n\t}'));

		const NOT_TRANSIENT: Record<string, string> = {
			loading: "owned by load()'s seq-gated finally",
			dashboard: 'page DATA — dropped by the keyed load effect before it reloads',
			dashError: 'page DATA — dropped by the keyed load effect before it reloads',
			dashboardSlug: 'page DATA — dropped by the keyed load effect before it reloads',
			collections: 'page DATA — dropped by the keyed load effect before it reloads',
			isOwner: 'a sticky permission, reset by its own (user, workspace)-keyed effect',
			justCreatedSlugs: 'the aha-highlight track, dropped by the keyed load effect with the data',
			onboardingDismissed:
				'a per-workspace preference in localStorage: it belongs to the browser, not to ' +
				'either identity, and carries nothing about the previous session',
		};

		// The type annotation may not span a line (`[^=\n]`, not `[^=]`): with
		// the wider class, `let pollTimer: ReturnType<…> | undefined;` on the
		// line ABOVE a `$state` declaration matched as ONE declaration named
		// `pollTimer`, and the `$state` on the next line was never enumerated
		// at all — an enumeration hole that reported the wrong name and hid the
		// right one (found when this guard first ran against this page).
		const declared = [...CODE.matchAll(/^\s*let\s+(\w+)\s*(?::[^=\n]*)?=\s*\$state/gm)].map((m) => m[1]!);
		expect(declared.length, 'no $state declarations found — re-point this guard').toBeGreaterThan(5);

		const undispositioned = declared.filter(
			(n) => !(n in NOT_TRANSIENT) && !new RegExp(`\\b${n}\\s*=`).test(resetBody)
		);
		expect(
			undispositioned,
			'these $state values are neither page data nor reset by resetTransientState(). Each is ' +
				'transient interaction state that survives an identity change: decide which it is and ' +
				'either reset it or name it in this guard.'
		).toEqual([]);

		// And the reset must still DO something — a table can be satisfied by
		// naming everything as not-transient.
		for (const v of ['connectOpen', 'showCreateCollection']) {
			expect(resetBody, `${v} is no longer reset on an identity change`).toMatch(new RegExp(`\\b${v}\\s*=\\s*false`));
		}
	});

	it('no $effect depends on the identity epoch without saying so', () => {
		// The family rule (checkpoint 18). A DISPOSITION TABLE, not a ban — and
		// on this surface it is EMPTY, which is the state that needs no
		// argument: no effect reads the epoch, and the one that dispatches
		// `load()` does so untracked (asserted positionally above).
		//
		// An entry added here names WHICH reads it covers and, where the reason
		// is positional, an `afterMarker` — and it owes a control that puts the
		// defect back through it (the core's header, rule 3).
		const INTENDED_DEPENDENCY: Record<string, { allowedReads: string[]; why: string; afterMarker?: string }> = {};

		const offenders: string[] = [];
		for (const block of src.effectBlocks()) {
			const entry = Object.entries(INTENDED_DEPENDENCY).find(([k]) => block.body.includes(k));
			const markerAt = entry?.[1].afterMarker ? block.body.indexOf(entry[1].afterMarker) : -1;
			for (const read of trackedEpochReadDetails(block.body)) {
				const positionOk = markerAt === -1 || read.index > markerAt;
				if (entry && positionOk && entry[1].allowedReads.some((t) => read.token.startsWith(t))) continue;
				offenders.push(`${block.label}: ...${read.context}`);
			}
		}
		expect(
			offenders,
			'these $effects read the identity epoch without untracking THAT READ and without a ' +
				'disposition vouching for it. Each re-runs on an identity change, re-creating whatever it ' +
				"armed — under the new epoch, over the previous user's state."
		).toEqual([]);
		expect(src.effectBlocks().length, 'no $effect found — re-point this guard').toBeGreaterThan(0);
	});
});
