/**
 * BUG-3084 surface 3 — the SOURCE half. `libraryIdentityFence.svelte.test.ts`
 * beside this file owns the SEMANTICS; this owns the POPULATION.
 *
 * Separating the two is the reusable part of this family. The behavioural suite
 * can only speak for the handlers it drives; this one enumerates every commit
 * point on the page and fails when a new member arrives, which is what stops a
 * later handler being added without a fence and nobody noticing.
 *
 * Every rule here is the family's, carried from surfaces 1 and 2 rather than
 * re-derived — including the two that were learned the hard way:
 *   - the re-stamp must come AFTER the state it vouches for, and on every path
 *     that leaves the page usable (roles board checkpoint 14);
 *   - a continuation that has LOST the identity writes no shared interaction
 *     state, because the identity listener already reset it and someone else
 *     owns it now (roles board, codex round 4).
 */
import { describe, it, expect } from 'vitest';
import { readFenceSource, withoutCatchArms, trackedEpochReadDetails } from '../../../../test/identityFenceSource';

const src = readFenceSource(new URL('./+page.svelte', import.meta.url));
const CODE = src.code;

describe('the library page fences every async commit point', () => {
	it('declares the page-level fence helpers, re-stamped after the load lands', () => {
		expect(
			CODE,
			'identityEpochAtLoad is not $state, so the $effect reading pageIdentityHeld() would not ' +
				're-run when the epoch is re-stamped'
		).toMatch(/let\s+identityEpochAtLoad\s*=\s*\$state\(authStore\.identityEpoch\)/);
		expect(CODE).toMatch(
			/function captureIdentity\(\)\s*:\s*number\s*\{[\s\S]*?return authStore\.identityEpoch/
		);
		expect(CODE).toMatch(
			/function identityHeld\(captured: number\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === captured/
		);
		expect(CODE).toMatch(
			/function pageIdentityHeld\(\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === identityEpochAtLoad/
		);

		// THE RE-STAMP POSITION, bounded on BOTH sides because each bound has
		// its own failure mode: too early and the guard vouches for data it has
		// not replaced; missing on a path and a transient error pins the fence
		// shut for ever.
		const load = src.asyncFunctions().get('loadData');
		expect(load, 'loadData() was renamed — re-point this guard').toBeDefined();

		const firstAwait = load!.indexOf('await');
		expect(firstAwait, 'loadData no longer awaits — this guard measures nothing').toBeGreaterThan(-1);
		expect(
			load!.slice(0, firstAwait),
			'loadData re-stamps identityEpochAtLoad BEFORE its await: every fence reading it then ' +
				"vouches for the previous session's data for the whole round-trip"
		).not.toMatch(/identityEpochAtLoad\s*=/);

		const tail = load!.slice(firstAwait);
		const restamp = tail.indexOf('identityEpochAtLoad =');
		expect(restamp, 'loadData never re-stamps — the fence can never recover').toBeGreaterThan(-1);
		expect(
			tail.slice(0, restamp),
			'the epoch is re-stamped before the sets the activate handlers read are replaced'
		).toContain('activeConventionTitles = new Set(');
		expect(
			(tail.match(/identityEpochAtLoad\s*=/g) ?? []).length,
			'identityEpochAtLoad is re-stamped on only one path out of loadData — the other leaves ' +
				'the page permanently fenced against its own user'
		).toBe(2);
	});

	it('subscribes to identity changes, re-loads, resets, and unsubscribes', () => {
		expect(
			CODE,
			'the page does not subscribe to authStore.onIdentityChange. Its $effect is keyed on wsSlug ' +
				'alone, so nothing re-runs the load when the identity moves and pageIdentityHeld() ' +
				'refuses for ever after a sign-in'
		).toMatch(/authStore\.onIdentityChange\(/);

		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const listener = sub.slice(0, sub.indexOf('\t});'));
		expect(listener, 'the identity-change listener does not re-run loadData').toMatch(/loadData\(/);
		expect(
			listener,
			'the listener does not reset transient interaction state: activatingTitle gates both ' +
				'activate handlers at their first line, so a stale value latches them shut'
		).toMatch(/resetTransientState\(\)/);
		expect(CODE).toMatch(/onDestroy\(stopIdentityWatch\)/);
	});

	it('the recovery never leaves a fence vouching for state it has not replaced', () => {
		// THE FAMILY PROPERTY, satisfied two ways. Surface 1 (#1374) and
		// settings (#1370) clear before re-loading; the roles board and this
		// page re-stamp late. Asserted as a disjunction so a later surface may
		// hold it either way — but NEITHER is the failure this catches.
		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const listener = sub.slice(0, sub.indexOf('\t});'));
		const clearsFirst =
			/activeConventionTitles\s*=\s*new Set\(\)/.test(listener) &&
			listener.indexOf('activeConventionTitles') < listener.indexOf('loadData(');

		const load = src.asyncFunctions().get('loadData')!;
		const firstAwait = load.indexOf('await');
		const tail = firstAwait > -1 ? load.slice(firstAwait) : '';
		const restamp = tail.indexOf('identityEpochAtLoad =');
		const restampsLate =
			firstAwait > -1 &&
			!/identityEpochAtLoad\s*=/.test(load.slice(0, firstAwait)) &&
			restamp > -1 &&
			tail.slice(0, restamp).includes('activeConventionTitles = new Set(');

		expect(
			clearsFirst || restampsLate,
			'the identity-change reload neither clears the state the page-level fence vouches for nor ' +
				'defers the re-stamp until the new data lands. Either is fine; neither means every ' +
				"pageIdentityHeld() during the reload answers TRUE over the previous session's data."
		).toBe(true);
	});

	it('fences the handlers that WRITE state the previous identity chose', () => {
		// The roles board's handleDndFinalize class. An entry capture asks "has
		// the identity moved since this work started"; these handlers' work
		// starts with a CLICK, always the current epoch, over a library entry
		// chosen against the list the PREVIOUS identity loaded.
		const MUST_HOLD_THE_PAGE = {
			activateConvention: 'activates a convention chosen from the previous list',
			activatePlaybook: 'activates a playbook chosen from the previous list',
		};
		const missing: string[] = [];
		for (const [name, why] of Object.entries(MUST_HOLD_THE_PAGE)) {
			const body = src.asyncFunctions().get(name);
			expect(body, `${name}() was renamed — re-point this guard`).toBeDefined();
			const head = body!.indexOf('await') > -1 ? body!.slice(0, body!.indexOf('await')) : body!;
			if (!head.includes('pageIdentityHeld()')) missing.push(`${name} (${why})`);
		}
		expect(
			missing,
			'these handlers write state the PREVIOUS identity chose but check only an entry capture, ' +
				'which a click after the identity change always passes'
		).toEqual([]);
	});

	it('fences every DEFERRED TIMER, which is why this population is 7 and not 3', () => {
		// The four timers are the members a function-level count cannot see, and
		// the filed survey missed all four (BUG-3084 checkpoint 6). Each fires
		// three seconds after its handler returned and writes `toast` — a commit
		// point whose whole nature is to land late.
		const timers = src.deferredTimers();
		expect(timers.length, 'the timer population changed — disposition the new member').toBe(4);
		const unfenced = timers
			.filter((t) => !/identityHeld\(/.test(t.body))
			.map((t) => t.body.slice(0, 60).replace(/\s+/g, ' '));
		expect(
			unfenced,
			'these deferred callbacks commit without checking the identity, so a timer armed by the ' +
				"previous session clears a toast belonging to whoever is signed in now"
		).toEqual([]);

		// AND THEY MUST STILL DO THEIR JOB (codex round 1 [P2]). A fence is only
		// half the requirement: a callback that checks the identity and then
		// clears nothing satisfies the assertion above while leaving the toast
		// on screen for ever. Fixing a stale-write bug by making the write never
		// happen is the failure this family already shipped once, in the shape
		// of a fence that latched shut (#1372).
		const inert = timers
			.filter((t) => !/toast = null/.test(t.body))
			.map((t) => t.body.slice(0, 60).replace(/\s+/g, ' '));
		expect(
			inert,
			'these deferred callbacks no longer clear the toast at all — a fence that removes the ' +
				'behaviour instead of conditioning it'
		).toEqual([]);
	});

	it('the identity-change listener resets every piece of transient interaction state', () => {
		// ENUMERATED rather than sampled (CONVE-35). On the roles board this
		// class produced findings in two consecutive review rounds before it was
		// enumerated; carried here so this surface does not repeat that.
		const reset = CODE.slice(CODE.indexOf('function resetTransientState()'));
		const resetBody = reset.slice(0, reset.indexOf('\n\t}'));

		const NOT_TRANSIENT: Record<string, string> = {
			identityEpochAtLoad: 'identity bookkeeping — only loadData may write it',
			categories: 'page DATA — loadData replaces it on both arms',
			playbookCategories: 'page DATA — loadData replaces it on both arms',
			activeConventionTitles: 'page DATA — loadData replaces it on both arms',
			activePlaybookTitles: 'page DATA — loadData replaces it on both arms',
			loading: "owned by loadData's generation-guarded finally",
			activeTab:
				'a view preference read from the URL, not interaction state: it belongs to the ' +
				'viewer and carries nothing about either identity',
		};

		const declared = [...CODE.matchAll(/^\s*let\s+(\w+)\s*(?::[^=]*)?=\s*\$state/gm)].map((m) => m[1]!);
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
	});

	it('a lost-identity exit writes no shared interaction state', () => {
		// The roles board's round-4 reversal, carried here so this surface never
		// acquires the defect it corrected. `resetTransientState()` owns these;
		// a stale continuation writing them again can only clobber what the NEW
		// user is doing.
		const SHARED = ['activatingTitle', 'toast'];
		const offenders: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (name === 'loadData') continue;

			for (const m of body.matchAll(/if \(!(?:page)?[Ii]dentityHeld\([^)]*\)\)\s*\{([\s\S]*?)\}/g)) {
				const arm = m[1] ?? '';
				for (const v of SHARED) {
					if (new RegExp(`\\b${v}\\s*=`).test(arm)) offenders.push(`${name}: ${v} (lost-identity arm)`);
				}
			}

			// A continuation — after an await — must be checked SINCE that await.
			// "Since the last await", not "on the same line": both spellings are
			// correct and the family uses both (CONVE-30).
			for (const m of body.matchAll(
				new RegExp(`\\b(${SHARED.join('|')})\\s*=\\s*(?:null|false|true)`, 'g')
			)) {
				const at = m.index ?? 0;
				const before = body.slice(0, at);
				const lastAwait = before.lastIndexOf('await ');
				if (lastAwait === -1) continue;
				if (/identityHeld\(/.test(body.slice(lastAwait, at + m[0].length))) continue;
				offenders.push(`${name}: ${m[1]} (continuation with no identity check since its await)`);
			}
		}
		expect(
			offenders,
			'these lost-identity exits write shared interaction state that the identity listener has ' +
				'already reset, so they can clobber what the NEW user is doing'
		).toEqual([]);
	});

	it("a stale activation does not release the new identity's activate gate", () => {
		// The `finally` counterpart, and it needs its own assertion because the
		// continuation rule above CANNOT see it: that rule asks whether an
		// identity check appears between the last await and the write, and a
		// `finally` always has the success-path check somewhere in that window.
		// The window is wide enough to be satisfied by a check that does not
		// govern this write. Rather than pretend a regex can express "which
		// check governs which statement", the two `finally` arms are named.
		// (Found by mutant L6 surviving the first version of these assertions —
		// the same way the roles board's M22 was found.)
		//
		// A deliberate ASYMMETRY with `loading` in loadData, which stays
		// unconditional: nothing else clears `loading` and pinning the page at
		// its skeleton is the worse failure, whereas `activatingTitle` IS
		// cleared by `resetTransientState()` on every identity change — so a
		// stale continuation clearing it again can only release a gate the NEW
		// user's own click is holding, permitting a second concurrent activation.
		for (const name of ['activateConvention', 'activatePlaybook']) {
			const body = src.asyncFunctions().get(name);
			expect(body, `${name}() was renamed — re-point this guard`).toBeDefined();
			const fin = body!.indexOf('} finally {');
			expect(fin, `${name} no longer has a finally arm — re-point this guard`).toBeGreaterThan(-1);
			expect(
				body!.slice(fin),
				`${name} clears activatingTitle unconditionally, so a stale request releases the ` +
					"new user's activate gate behind their back"
			).toMatch(/if \(identityHeld\(epochAtEntry\)\) activatingTitle = null/);
		}
	});

	it('the load effect is keyed on the workspace, not on the identity', () => {
		// `loadData` reads `authStore.identityEpoch` synchronously before its
		// first await, so an un-untracked effect takes a dependency on the epoch
		// and re-runs on every identity change — on top of the listener that
		// already re-loads for that event (codex round 1 [P2]). Correct on
		// screen, double on the wire.
		const eff = CODE.slice(CODE.indexOf('$effect(() => {'));
		const body = eff.slice(0, eff.indexOf('\t});'));
		expect(
			body,
			'the load effect calls loadData tracked, so it re-runs on an identity change as well as ' +
				'a workspace change and every load is issued twice'
		).toMatch(/untrack\(\(\) => loadData\(/);
	});

	it('a stale load may not write the page it no longer owns', () => {
		// Different question from the identity fence, and neither implies the
		// other: both loads can belong to the SAME user, which is exactly what a
		// workspace switch produces on this page.
		const load = src.asyncFunctions().get('loadData')!;
		expect(load, 'loadData does not take a load generation').toMatch(/const myLoad = \+\+loadGen/);
		expect(
			(load.match(/if \(myLoad !== loadGen\) return;/g) ?? []).length,
			'loadData does not gate its state writes on the load generation, so a stale load can ' +
				'overwrite a newer one for the same user'
		).toBeGreaterThanOrEqual(2);
		expect(load.indexOf('if (myLoad !== loadGen) return;')).toBeLessThan(
			load.indexOf('categories = libraryRes.categories')
		);
		expect(
			load,
			'loadData clears `loading` unconditionally, so a stale load can declare a newer one finished'
		).toMatch(/if \(myLoad === loadGen\) loading = false/);
	});

	it('the error path clears the state its own re-stamp will vouch for', () => {
		const load = src.asyncFunctions().get('loadData')!;
		const cat = load.indexOf('} catch {');
		expect(cat, 'loadData no longer has a catch arm — re-point this guard').toBeGreaterThan(-1);
		const arm = load.slice(cat, load.indexOf('} finally {', cat));
		expect(arm, 'a failed load leaves the previous identity\'s convention set in place').toMatch(
			/activeConventionTitles = new Set\(\)/
		);
		expect(arm, 'a failed load leaves the previous identity\'s playbook set in place').toMatch(
			/activePlaybookTitles = new Set\(\)/
		);
		expect(arm, 'a failed load never re-stamps, pinning the fence shut for ever').toMatch(
			/identityEpochAtLoad\s*=/
		);
		expect(arm.indexOf('activeConventionTitles = new Set()')).toBeLessThan(
			arm.indexOf('identityEpochAtLoad =')
		);
	});

	it('no $effect depends on the identity epoch without saying so', () => {
		// The family rule (BUG-3084 checkpoint 18). An `$effect` reading
		// `authStore.identityEpoch` SYNCHRONOUSLY re-runs on every identity
		// change, re-creating whatever it armed under the new epoch. On the
		// collection page that re-armed a debounce with the previous user's
		// typed text; here it produced a second load per identity change.
		//
		// A DISPOSITION TABLE, not a ban — two of this family's three tracked
		// reads want the dependency. This surface's one effect is untracked, so
		// its table is empty, which is the state that needs no argument.
		// DISPOSITIONED PER EFFECT, and each entry names WHICH READS it covers.
		//
		// Exempting a whole effect was wrong and hid this unit's own defect: the
		// search entry skipped the synchronous capture along with the timer's
		// reads, so removing that capture's `untrack` produced ZERO offenders
		// (codex round 2 [P2]). An entry now lists the read tokens it vouches
		// for, and any other read in that effect is an offender.
		const INTENDED_DEPENDENCY: Record<string, { allowedReads: string[]; why: string; afterMarker?: string }> = {};

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
	});

	it('enumerates the population it claims to cover', () => {
		// COUNTS ASSERTED so a new member cannot arrive unnoticed. These are the
		// widened instrument's four populations; the originally filed survey saw
		// only the first and reported 3 (BUG-3084 checkpoint 6).
		expect(src.asyncFunctions().size, 'a top-level async function was added or removed').toBe(3);
		expect(src.markupAsyncArrows(), 'an inline async arrow appeared in the markup').toHaveLength(0);
		expect(src.nestedAsyncCallbacks(), 'an async callback arrived').toHaveLength(0);
		expect(src.deferredTimers(), 'a setTimeout/setInterval arrived or left').toHaveLength(4);
	});

	it('captures the identity at ENTRY in every async handler, before its first await', () => {
		const missing: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (!body.includes('await')) continue;
			if (!body.slice(0, body.indexOf('await')).includes('captureIdentity()')) missing.push(name);
		}
		expect(
			missing,
			'these async handlers do not capture the identity before their first await. A fence ' +
				'comparing against anything else can be defeated by a concurrent load re-stamping it.'
		).toEqual([]);
	});

	it('never lets a handler compare against the PAGE load epoch after an await', () => {
		// `pageIdentityHeld()` is legitimate BEFORE the await — it is the
		// "does this page's data belong to the signed-in user" question. After
		// an await it is the wrong instrument, because a concurrent load can
		// re-stamp the value it reads to exactly what the handler expects.
		const offenders: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (name === 'loadData') continue;
			const first = body.indexOf('await');
			if (first === -1) continue;
			if (body.slice(first).includes('pageIdentityHeld()')) offenders.push(name);
		}
		expect(
			offenders,
			'these handlers compare against the PAGE load epoch after an await, which a concurrent ' +
				'load can re-stamp to the value the handler is about to read'
		).toEqual([]);
	});

	it('checks the fence at least once per await in every async handler', () => {
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
			'these async handlers have fewer identity checks than awaits, so at least one continuation ' +
				'commits unguarded'
		).toEqual([]);
	});

	it('guards the SUCCESS continuation, not merely the failure one', () => {
		// The window is the body with every CATCH ARM EXCISED. Splitting at the
		// first `} catch` reports correct code as unguarded when a handler
		// commits after its try/catch; balancing arms separately does the same
		// for an await in a nested try. Both were tried on surface 1.
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
			'these handlers have no identity check between their first await and the end of their try ' +
				'block, so the path a SUCCESSFUL request takes commits unguarded'
		).toEqual([]);
	});
});
