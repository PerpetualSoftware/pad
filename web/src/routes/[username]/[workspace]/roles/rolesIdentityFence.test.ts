// Node-project SOURCE guard: every async commit point on the roles board is
// fenced on the signed-in identity (BUG-3084, surface 2 of 7).
//
// THE GATE BESIDE THIS (TASK-3097): `rolesIdentityGate.test.ts` tables every async unit on
// this page with the hash of the code it was reviewed on, and refuses any
// edit to one until its row is re-read. That catches BUG-3084's round-4
// classes, which a source scanner cannot see. This file catches what the
// gate does not: the RULE on a NEW handler, which the gate accepts with any
// row. Keep both. Neither is a duplicate of the other.
//
// Same pair as surface 1: `rolesIdentityFence.svelte.test.ts` beside this file
// owns the SEMANTICS of the handlers it drives; this owns the POPULATION,
// because a behavioural suite only covers the members someone wrote a case for
// and cannot say a seventh handler arrived unfenced.
//
// WHY THIS SURFACE WENT SECOND, ahead of larger ones: it does not merely
// REPORT stale results, it COMPOSES A WRITE from state captured under the
// previous identity. `handleDndFinalize` sets `update.assigned_user_id =
// currentUserId`, and `currentUserId` is read from `/auth/session` by
// `loadData` — so after an identity change a drag PERSISTS the previous user's
// id as an assignee. The server cannot refuse it, because the signed-in user
// may legitimately assign to anyone. That is a wrong row, not a misleading
// screen, and it is why one assertion below is about a check's position
// relative to a REQUEST rather than relative to a commit.
//
// The conditional-reload argument that makes a fence necessary at all is
// surface 1's and is not repeated: `routes/+layout.svelte` does not reload on a
// sign-IN from anonymous and does not reload on sign-out.
//
// WHAT A SOURCE GUARD CANNOT DO: it checks spellings. A fence comparing the
// wrong two values, one made unreachable by an earlier return, or one placed
// after the commit it guards all pass here — except the two specific
// placements asserted explicitly below. That is the behavioural suite's job.
import { describe, it, expect } from 'vitest';
import { readFenceSource, withoutCatchArms, trackedEpochReadDetails } from '../../../../test/identityFenceSource';

const src = readFenceSource(new URL('./+page.svelte', import.meta.url));
const CODE = src.code;

/**
 * Handlers that legitimately need no ENTRY CAPTURE, with the reason. A handler
 * lands here only when capturing at its own entry would be WRONG — not when a
 * fence merely looks unnecessary.
 */
// EMPTY, and that is the corrected state (BUG-3084 checkpoint 14). `loadData`
// was exempted here on the grounds that it WRITES `identityEpochAtLoad` rather
// than reading it. That was true and still let the defect through: it writes
// that value, and the question the exemption never asked is WHEN. It wrote it
// before its own await, so the page vouched for itself for the whole round-trip
// while holding the previous identity's data. `loadData` now captures at entry
// like every other handler and re-stamps at the end, so it needs no exemption —
// and an exemption list that is empty is one nobody has to audit.
const NO_ENTRY_CAPTURE: Record<string, string> = {};

describe('the roles board fences every async commit point', () => {
	it('declares the page-level fence helpers, re-stamped at load', () => {
		// `$state(...)` REQUIRED, not merely accepted. The first version of this
		// assertion allowed either spelling and mutant M13 — dropping `$state` —
		// SURVIVED the whole suite, which is how the hole was found rather than
		// argued.
		//
		// Why no behavioural leg covers it, stated because "the guard is the
		// only instrument" is normally a smell: the `$effect` fencing the
		// laneData sync reads `authStore.identityEpoch` (reactive) through
		// `pageIdentityHeld()`, so it still re-runs when the identity MOVES. What
		// an untracked `identityEpochAtLoad` costs is the re-run when the epoch
		// is RE-STAMPED — and the effect happens to recover anyway, because
		// `lanes` is replaced in the same load and `orderedLanes` is a dependency
		// it already has. So the untracked version is correct today by
		// coincidence of an unrelated dependency, which is exactly the kind of
		// correctness that disappears in a refactor nobody connects to this file.
		// The guard states the requirement instead of relying on the coincidence.
		expect(
			CODE,
			'identityEpochAtLoad is not $state, so the $effect reading pageIdentityHeld() is not ' +
				're-run when the epoch is re-stamped'
		).toMatch(/let\s+identityEpochAtLoad\s*=\s*\$state\(authStore\.identityEpoch\)/);
		expect(CODE).toMatch(
			/function captureIdentity\(\)\s*:\s*number\s*\{[\s\S]*?return authStore\.identityEpoch/
		);
		expect(CODE).toMatch(
			/function identityHeld\(captured: number\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === captured/
		);
		// `pageIdentityHeld()` HAS callers on this surface — both drag handlers.
		// (This comment previously said it had none; that was written before the
		// behavioural suite proved otherwise and is corrected here per CONVE-23,
		// since leaving it would tell the next reader the opposite of the fact
		// the file is built on.)
		expect(CODE).toMatch(
			/function pageIdentityHeld\(\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === identityEpochAtLoad/
		);

		// THE RE-STAMP POSITION, which this guard used to assert BACKWARDS.
		//
		// It required `identityEpochAtLoad = authStore.identityEpoch` BEFORE the
		// first await — pinning the defect in place as though it were the
		// contract. `pageIdentityHeld()` answers "does this page's DATA belong to
		// the signed-in user", and `handleDndFinalize` composes a write from that
		// data, so re-stamping before the await makes the page vouch for itself
		// for the length of the round-trip while `currentUserId` and `lanes` are
		// still the PREVIOUS session's. Measured, not argued: with the old
		// position and a board that stays rendered while loading, a drag inside
		// that window persisted the previous user's id as the assignee.
		//
		// The corrected property is bounded on BOTH sides, because each bound
		// has its own failure mode:
		//   - re-stamp AFTER the awaited state has been assigned, or the guard
		//     vouches for data it has not replaced (this bug);
		//   - re-stamp on every path that leaves the page usable, or a transient
		//     error pins the fence shut for ever (#1372's regression, repaired
		//     in #1374).
		const load = src.asyncFunctions().get('loadData');
		expect(load, 'loadData() was renamed — re-point this guard').toBeDefined();

		const firstAwait = load!.indexOf('await');
		expect(firstAwait, 'loadData no longer awaits — this guard measures nothing').toBeGreaterThan(-1);
		expect(
			load!.slice(0, firstAwait),
			'loadData re-stamps identityEpochAtLoad BEFORE its await: every fence reading it then ' +
				'vouches for the previous session\'s data for the whole round-trip'
		).not.toMatch(/identityEpochAtLoad\s*=/);

		// After the await, and after the state it vouches for is assigned.
		const tail = load!.slice(firstAwait);
		const restamp = tail.indexOf('identityEpochAtLoad =');
		expect(restamp, 'loadData never re-stamps identityEpochAtLoad — the fence can never recover').toBeGreaterThan(-1);
		expect(
			tail.slice(0, restamp),
			'the epoch is re-stamped before currentUserId is replaced — the window this bug is about'
		).toContain('currentUserId = session.user.id');

		// Both arms. A re-stamp only on success leaves a failed reload inert.
		expect(
			(tail.match(/identityEpochAtLoad\s*=/g) ?? []).length,
			'identityEpochAtLoad is re-stamped on only one path out of loadData — the other leaves ' +
				'the page permanently fenced against its own user'
		).toBe(2);
	});

	it('subscribes to identity changes, re-loads, and unsubscribes', () => {
		// Ported from #1374's collection guard (owed item 4). Without this the
		// page's only re-stamp site never runs on the event it exists for, and
		// `pageIdentityHeld()` refuses for ever after a sign-in — the regression
		// #1372 shipped past four codex rounds and a 10/10 matrix.
		expect(
			CODE,
			'the page does not subscribe to authStore.onIdentityChange, so a stale load epoch is never ' +
				'refreshed and pageIdentityHeld() refuses for ever after a sign-in'
		).toMatch(/authStore\.onIdentityChange\(/);

		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const body = sub.slice(0, sub.indexOf('\t});'));
		expect(
			body,
			'the identity-change listener does not re-run loadData, so the load epoch stays stale'
		).toMatch(/loadData\(/);
		expect(CODE).toMatch(/onDestroy\(stopIdentityWatch\)/);
	});

	it('the recovery never leaves a fence vouching for state it has not replaced', () => {
		// THE PROPERTY, stated once and satisfied two ways — which is the
		// reconciliation owed item 1 asked for between this branch and #1374
		// (BUG-3084 checkpoint 14). They do not reconcile on code, and forcing
		// them to would have meant shipping lines this suite proved inert.
		//
		// Between an identity change and the reload landing, a page holds the
		// PREVIOUS identity's data. Anything reading `pageIdentityHeld()` in that
		// window is being told the page belongs to the signed-in user. On this
		// surface that reader is `handleDndFinalize`, which stamps
		// `currentUserId` into `assigned_user_id` — so a page that vouches for
		// itself too early persists a wrong ROW, not a wrong screen.
		//
		// Two ways to hold the property:
		//   (a) CLEAR the state before re-loading, so there is nothing stale for
		//       the fence to vouch for. Surface 1 (#1374) and settings (#1370)
		//       do this, because their markup keeps state on screen mid-reload.
		//   (b) RE-STAMP LATE, so the fence answers false until the new data has
		//       landed. This surface does this.
		//
		// Asserted as a disjunction rather than picking one, because a later
		// surface may legitimately need either — but NEITHER is a failure, and
		// that is the case this test exists to catch.
		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const listener = sub.slice(0, sub.indexOf('\t});'));
		// BOTH values, not just `currentUserId` (codex round 3 [P2]): a listener
		// clearing the id while leaving `lanes` still hands the fence a board the
		// previous identity loaded.
		const clearMatch = listener.match(/currentUserId\s*=\s*(?:null|'')/);
		const lanesMatch = listener.match(/lanes\s*=\s*\[\]/);
		const clearsFirst =
			!!clearMatch &&
			!!lanesMatch &&
			listener.indexOf(clearMatch[0]) < listener.indexOf('loadData(') &&
			listener.indexOf(lanesMatch[0]) < listener.indexOf('loadData(');

		// STRENGTHENED after codex round 2 [P2] said so, and it was right: this
		// used to prove only that no re-stamp appears before the first await,
		// which a re-stamp placed after the await but BEFORE the state it
		// vouches for also satisfies — the exact defect, passing its own guard.
		// It now requires the re-stamp to follow the assignment of both values a
		// reader of `pageIdentityHeld()` on this page depends on.
		const load = src.asyncFunctions().get('loadData')!;
		const firstAwait = load.indexOf('await');
		const tail = firstAwait > -1 ? load.slice(firstAwait) : '';
		const restamp = tail.indexOf('identityEpochAtLoad =');
		const restampsLate =
			firstAwait > -1 &&
			!/identityEpochAtLoad\s*=/.test(load.slice(0, firstAwait)) &&
			restamp > -1 &&
			tail.slice(0, restamp).includes('currentUserId = session.user.id') &&
			tail.slice(0, restamp).includes('lanes = boardResult.lanes');

		expect(
			clearsFirst || restampsLate,
			'the identity-change reload neither clears the state the page-level fence vouches for nor ' +
				'defers the re-stamp until the new data lands. Either is fine; neither means every ' +
				'pageIdentityHeld() during the reload answers TRUE over the previous session\'s data, ' +
				'and on this surface that persists the previous user as an assignee.'
		).toBe(true);
	});

	it('fences the handlers that WRITE state the previous identity chose', () => {
		// The `handleDndFinalize` class, swept rather than sampled (CONVE-18).
		// An entry capture asks "has the identity moved since this work started";
		// these handlers' work starts with a CLICK, which is always the current
		// epoch, over form state chosen against the previous identity's board.
		// So they need the page-level question too, and codex round 2 [P1] found
		// three of them still carrying only the entry capture.
		//
		// NAMED, not enumerated, and the reason matters: "every handler that
		// composes a write from earlier state" is not a set a grep produces, and
		// a rule that cannot enumerate its population should not pretend to. The
		// list is the audit; adding a handler that belongs here is a reading
		// task the population count below forces someone to do.
		const MUST_HOLD_THE_PAGE = {
			handleDndFinalize: 'stamps currentUserId into assigned_user_id',
			handleLaneDrop: 'reorders lanes loaded under the previous identity',
			saveRole: 'writes editingRoleId, taken from the previous board',
			deleteRole: 'DELETES editingRoleId, taken from the previous board',
			submitNewItem: 'creates into a collection chosen from the previous list',
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

	it('the identity-change listener resets every piece of transient interaction state', () => {
		// ENUMERATED rather than sampled, and the enumeration is the point
		// (CONVE-35). Rounds 2 and 3 each returned members of ONE class, found
		// one at a time — an open role modal, an open new-item modal, a stuck
		// `newItemSaving`, a stuck `isDragging`, stale lane-drag styling. A loop
		// that keeps finding new members of a class is measuring the enumeration,
		// not the code, so the fix is to enumerate.
		//
		// Two reasons the class matters, and they are different failures:
		//   - DISCLOSURE: state mounted outside `{#if loading}` stays on screen
		//     for whoever signs in next.
		//   - LATCHING: `isDragging` disables the effect that repopulates
		//     `laneData`, and the loading branch destroys the dnd zones without
		//     dispatching `finalize`, so a drag interrupted by an identity change
		//     leaves it true with nothing left to clear it. The board then never
		//     repopulates — #1372's failure mode, in a new place.
		const sub = CODE.slice(CODE.indexOf('authStore.onIdentityChange('));
		const listener = sub.slice(0, sub.indexOf('\t});'));
		expect(
			listener,
			'the identity-change listener does not reset the page\'s transient interaction state'
		).toMatch(/resetTransientState\(\)/);

		const reset = CODE.slice(CODE.indexOf('function resetTransientState()'));
		const resetBody = reset.slice(0, reset.indexOf('\n\t}'));

		// Every `$state` on the page, dispositioned. A new one fails HERE and
		// has to be looked at, which is the whole point of enumerating.
		const DATA_OR_IDENTITY: Record<string, string> = {
			identityEpochAtLoad: 'identity bookkeeping — only loadData may write it',
			lanes: 'page DATA — loadData replaces it, and clears it on the error arm',
			currentUserId: 'page DATA — loadData replaces it, and clears it when unauthenticated',
			loading: 'owned by loadData\'s generation-guarded finally',
			error: 'owned by loadData, set and cleared there',
			highlightMine: 'a view preference, not interaction state: it belongs to the viewer, ' +
				'carries nothing about either identity, and resetting it would be a surprise',
			newItemTitleInput: 'a DOM element binding, not state — Svelte clears it on unmount',
		};
		// Reached through closeModal() / closeNewItem(), which the reset calls.
		const VIA_CLOSERS = [
			'newItemOpen', 'newItemCollectionSlug', 'newItemTitle',
			'roleDialogOpen', 'dialogMode', 'editingRoleId',
			'editName', 'editDescription', 'editIcon', 'editTools',
		];

		// Enumerated by the core, by STATEMENT (BUG-3084 M12): the regex this
		// guard carried let a type annotation run across a newline into the
		// next statement, so an uninitialised typed `let` above a `$state` line
		// swallowed it. No such pair here today; the hole was real regardless.
		const declared = src.stateDeclarations();
		expect(declared.length, 'no $state declarations found — re-point this guard').toBeGreaterThan(10);

		const undispositioned = declared.filter(
			(n) =>
				!(n in DATA_OR_IDENTITY) &&
				!VIA_CLOSERS.includes(n) &&
				!new RegExp(`\\b${n}\\s*=`).test(resetBody)
		);
		expect(
			undispositioned,
			'these $state values are neither page data, nor reset by resetTransientState(), nor reached ' +
				'through the closers it calls. Each is transient interaction state that survives an ' +
				'identity change: decide which it is and either reset it or name it in this guard.'
		).toEqual([]);

		expect(resetBody, 'the role modal stays open across an identity change').toMatch(/closeModal\(\)/);
		expect(resetBody, 'the new-item modal stays open across an identity change').toMatch(/closeNewItem\(\)/);
		expect(
			resetBody,
			'isDragging is not released, so the effect repopulating laneData stays disabled for ever'
		).toMatch(/isDragging = false/);
	});

	it('a lost-identity exit writes no shared interaction state', () => {
		// The rule REPLACES round 1's, and the reversal is the interesting part
		// (codex round 4 [P2]). Round 1 required every fenced exit in
		// `handleDndFinalize` to release `isDragging`, because nothing else
		// would and the board would pin. Round 3 gave the reset to the identity
		// listener, which runs synchronously on the epoch bump and therefore
		// always before a stale continuation resumes — at which point the same
		// release became a way for a previous-identity continuation to cancel a
		// drag the NEW user had started.
		//
		// So: a continuation that has lost the identity may not write shared
		// interaction state. The fix for the latch it used to guard now lives in
		// one place instead of at every exit.
		const SHARED = ['isDragging', 'newItemSaving', 'draggedLaneKey', 'dragOverLaneKey', 'laneData'];
		const offenders: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (name === 'loadData') continue;

			// (a) Inside a lost-identity guard arm.
			for (const m of body.matchAll(/if \(!(?:page)?[Ii]dentityHeld\([^)]*\)\)\s*\{([\s\S]*?)\}/g)) {
				const arm = m[1] ?? '';
				for (const v of SHARED) {
					if (new RegExp(`\\b${v}\\s*=`).test(arm)) offenders.push(`${name}: ${v} (lost-identity arm)`);
				}
			}

			// (b) In a CONTINUATION — after an await — which is the same question
			// one line later and is where round 5 found the instance (a) could
			// not see: a check placed BEFORE an await says nothing about a change
			// DURING it.
			//
			// The rule is "checked since the last await", not "checked on the
			// same line". Both spellings are correct and the file uses both: an
			// `if (identityHeld(e)) x = false;` on the write itself, and an
			// `if (!identityHeld(e)) return;` a line or two above it. Requiring
			// the first flagged two correct writes in `handleLaneDrop`, which is
			// the guard measuring its own formatting preference rather than the
			// property (CONVE-30 — state the question before the result becomes
			// the answer).
			for (const m of body.matchAll(
				new RegExp(`\\b(${SHARED.join('|')})\\s*=\\s*(?:false|true|null|\\{\\})`, 'g')
			)) {
				const at = m.index ?? 0;
				const before = body.slice(0, at);
				const lastAwait = before.lastIndexOf('await ');
				if (lastAwait === -1) continue; // not a continuation
				const sinceAwait = body.slice(lastAwait, at + m[0].length);
				if (/identityHeld\(/.test(sinceAwait)) continue;
				offenders.push(`${name}: ${m[1]} (continuation with no identity check since its await)`);
			}
		}
		expect(
			offenders,
			'these lost-identity exits write shared interaction state that the identity listener has ' +
				'already reset, so they can clobber what the NEW user is doing'
		).toEqual([]);
	});

	it('a stale create does not re-enable the new identity\'s form', () => {
		// The `finally` counterpart of the rule above, and a deliberate
		// ASYMMETRY with `loadData`'s own busy flag (codex round 4 [P2], found
		// by mutant M22 surviving the first version of these assertions).
		//
		// `loading` is cleared unconditionally because nothing else clears it and
		// pinning the board at the skeleton is the worse failure. `newItemSaving`
		// IS cleared by `resetTransientState()` on every identity change, so a
		// stale continuation clearing it again can only re-enable a form the NEW
		// user is already filling in — which permits a duplicate submit.
		const submit = src.asyncFunctions().get('submitNewItem');
		expect(submit, 'submitNewItem() was renamed — re-point this guard').toBeDefined();
		const fin = submit!.indexOf('} finally {');
		expect(fin, 'submitNewItem no longer has a finally arm — re-point this guard').toBeGreaterThan(-1);
		expect(
			submit!.slice(fin),
			'submitNewItem clears newItemSaving unconditionally, so a stale request re-enables the ' +
				"new user's create form behind their back"
		).toMatch(/if \(identityHeld\(epochAtEntry\)\) newItemSaving = false/);
	});

	it('a stale load may not write the page it no longer owns', () => {
		// `loadGen` gated only `loading` when it arrived, which reads as though
		// the overlapping-load race were handled while a slow first load could
		// still overwrite a newer one's board (codex round 4 [P2]). The identity
		// fence does not cover this: both loads can belong to the SAME user.
		const load = src.asyncFunctions().get('loadData')!;
		const checks = (load.match(/if \(myLoad !== loadGen\) return;/g) ?? []).length;
		expect(
			checks,
			'loadData does not gate its state writes on the load generation, so a stale load can ' +
				'overwrite a newer one for the same user'
		).toBeGreaterThanOrEqual(2);
		// Before the first assignment on each arm, not after it.
		expect(load.indexOf('if (myLoad !== loadGen) return;')).toBeLessThan(
			load.indexOf('lanes = boardResult.lanes')
		);
	});

	it('an unauthenticated load clears the previous user id rather than keeping it', () => {
		// The success arm re-stamps the fence. If the session comes back
		// unauthenticated and `currentUserId` is left alone, the page then
		// vouches for itself while still holding someone for handleDndFinalize
		// to assign to (codex round 3 [P2]).
		const load = src.asyncFunctions().get('loadData')!;
		expect(
			load,
			'loadData has no branch clearing currentUserId when the session is not authenticated'
		).toMatch(/if \(!session\.authenticated \|\| !session\.user\)[\s\S]{0,400}?currentUserId = ''/);
	});

	it('only the newest load may declare the page loaded', () => {
		// Without a generation guard an OLDER loadData's unconditional `finally`
		// clears `loading` while the identity-change reload is still in flight,
		// and the markup paints the board over the previous identity's `lanes`
		// (codex round 2 [P1]).
		const load = src.asyncFunctions().get('loadData')!;
		expect(load, 'loadData does not take a load generation').toMatch(/const myLoad = \+\+loadGen/);
		expect(
			load,
			'loadData clears `loading` unconditionally, so a stale load can declare a newer one finished'
		).toMatch(/if \(myLoad === loadGen\) loading = false/);
	});

	it('the error path clears the state its own re-stamp will vouch for', () => {
		// The success arm replaces `lanes` and `currentUserId` before re-stamping.
		// The error arm replaces nothing, so it must clear them instead — and the
		// re-stamp is still owed, because omitting it pins the page inert for
		// ever on a transient network error (#1372's failure mode).
		const load = src.asyncFunctions().get('loadData')!;
		const cat = load.indexOf('} catch (err) {');
		const fin = load.indexOf('} finally {', cat);
		expect(cat, 'loadData no longer has a catch arm — re-point this guard').toBeGreaterThan(-1);
		const arm = load.slice(cat, fin);
		expect(arm, 'a failed reload leaves the previous identity\'s lanes in place').toMatch(/lanes = \[\]/);
		// `= ''` rather than `= null`: `currentUserId` is a `string`, and the
		// `&& currentUserId` test in handleDndFinalize treats both as absent.
		expect(arm, 'a failed reload leaves the previous identity\'s currentUserId in place').toMatch(/currentUserId = ''/);
		expect(arm, 'a failed reload never re-stamps, pinning the fence shut for ever').toMatch(/identityEpochAtLoad\s*=/);
		expect(arm.indexOf('lanes = []')).toBeLessThan(arm.indexOf('identityEpochAtLoad ='));
	});

	it('the laneData sync does not paint a board the page no longer holds', () => {
		// `handleDndFinalize` mutates `lanes` optimistically. If the identity
		// moves during the `await loadData()` on its failure path, that load
		// fences itself out and leaves the optimistic value, and the
		// unconditional `isDragging = false` afterwards re-enables this effect
		// (codex round 2 [P1]). The release must stay unconditional — a fence
		// that skips it pins the board for ever — so the sync is fenced instead.
		expect(
			CODE,
			'the laneData sync effect runs regardless of whether the page still holds the identity'
		).toMatch(/if \(!isDragging && pageIdentityHeld\(\)\)/);
	});

	it('no $effect depends on the identity epoch without saying so', () => {
		// The family rule (BUG-3084 checkpoint 18). `authStore.identityEpoch` is
		// `$state`, so an `$effect` reading it SYNCHRONOUSLY re-runs on every
		// identity change — which on the collection page re-armed a debounce
		// under the new epoch with the previous user's text.
		//
		// A DISPOSITION TABLE, not a ban: this surface's one tracked read WANTS
		// the dependency. `identityEpochAtLoad` was made `$state` on purpose so
		// the laneData sync re-evaluates when the identity moves; untracking it
		// would undo that. An undispositioned read fails.
		// DISPOSITIONED PER EFFECT, and each entry names WHICH READS it covers.
		//
		// Exempting a whole effect was wrong and hid this unit's own defect: the
		// search entry skipped the synchronous capture along with the timer's
		// reads, so removing that capture's `untrack` produced ZERO offenders
		// (codex round 2 [P2]). An entry now lists the read tokens it vouches
		// for, and any other read in that effect is an offender.
		const INTENDED_DEPENDENCY: Record<string, { allowedReads: string[]; why: string; afterMarker?: string }> = {
			'laneData = data': {
				allowedReads: ['pageIdentityHeld('],
				why:
					'the laneData sync MUST re-run when the identity moves — that is why ' +
					'identityEpochAtLoad is $state.',
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
	});

	it('enumerates the population it claims to cover', () => {
		// Asserted so a new member cannot arrive unnoticed. These four counts
		// are the widened enumeration's, and on THIS surface they agree with the
		// filed survey — unlike `library`, the dashboard and ItemDetail, where
		// the function-level count was short (BUG-3084 checkpoint 6).
		expect(src.asyncFunctions().size, 'a top-level async function was added or removed').toBe(6);
		expect(src.markupAsyncArrows(), 'an inline async arrow appeared in the markup').toHaveLength(0);
		expect(src.nestedAsyncCallbacks(), 'an async callback arrived').toHaveLength(0);
		expect(src.deferredTimers(), 'a setTimeout/setInterval arrived').toHaveLength(0);
	});

	it('captures the identity at ENTRY in every async handler, before its first await', () => {
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
		const readers: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (name === 'loadData') continue; // the re-stamper, i.e. the writer
			if (body.includes('identityEpochAtLoad')) readers.push(name);
		}
		expect(
			readers,
			'these handlers read the page load epoch directly. It is re-stamped by loadData(), so a ' +
				"handler comparing against it commits when a concurrent load moves it to the current " +
				"value. Capture at the handler's own entry instead."
		).toEqual([]);
	});

	it('checks the fence at least once per await in every async handler', () => {
		// A LOWER BOUND, not a proof — it cannot see where the checks sit. The
		// two placements that matter on this surface are asserted separately
		// below, and the rest is the behavioural suite's half.
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
		// Surface 1's mutant M7: a handler with a check in its catch satisfies
		// the count above while its success path commits unguarded.
		//
		// The window is the body with every CATCH ARM EXCISED, not the text
		// before the first `} catch` — surface 1 used the latter and it reports
		// correct code as unguarded whenever a handler commits AFTER its
		// try/catch rather than inside it, which `handleLaneDrop` here does.
		// Back-ported to surface 1's guard in the same change, so the family
		// does not carry two answers to one question.
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

	it('never ends a block with a fence, which guards nothing', () => {
		// Surface 1's mutant M10: a check after the last commit in its block is
		// dead code that satisfies every count above. Codex returned CLEAN on
		// one of those.
		const trailing = /if \(!(?:identityHeld\([^)]*\)|pageIdentityHeld\(\))\) return;\s*\}\s*$/;
		const offenders: string[] = [];
		for (const [name, body] of src.asyncFunctions()) {
			if (trailing.test(body)) offenders.push(name);
		}
		expect(
			offenders,
			'these blocks END with an identity check, so it follows every commit it was meant to guard'
		).toEqual([]);
	});

	it('gives both drag handlers the PAGE check as well as an entry capture', () => {
		// Not a stylistic preference — an entry capture cannot answer the
		// question these two have. They compose their writes from `currentUserId`
		// and `lanes`, loaded by `loadData` under whoever was signed in THEN, so
		// a drag STARTING after an identity change passes every entry capture
		// (it is the current epoch) and still writes the previous session's
		// assignee and lane order.
		//
		// This was not believed until `rolesIdentityFence.svelte.test.ts` failed
		// on it; the first version of this page's fence had entry captures only,
		// and its header comment asserted `pageIdentityHeld` had no caller here.
		for (const name of ['handleDndFinalize', 'handleLaneDrop']) {
			const body = src.asyncFunctions().get(name);
			expect(body, `${name} was renamed — re-point this guard`).toBeDefined();
			const beforeFirstAwait = body!.slice(0, body!.indexOf('await'));
			expect(
				beforeFirstAwait,
				`${name} does not check pageIdentityHeld() before its first await, so a drag begun ` +
					'after an identity change writes state loaded under the previous one'
			).toContain('pageIdentityHeld()');
		}
	});

	it('checks the identity BEFORE issuing the write that carries currentUserId', () => {
		// THE ASSERTION THIS SURFACE EXISTS FOR, and the only one in the family
		// whose position is fixed by what a REQUEST CONTAINS rather than by what
		// lands after it.
		//
		// `handleDndFinalize` composes `update.assigned_user_id = currentUserId`
		// and then issues it. `currentUserId` is the PREVIOUS session's, read
		// from `/auth/session` by `loadData`, so a check placed after the await
		// — which every other assertion in this file would accept — still lets
		// the wrong assignee reach the database. The server cannot catch it
		// either: assigning to another user is a legitimate operation.
		const dnd = src.asyncFunctions().get('handleDndFinalize');
		expect(dnd, 'handleDndFinalize was renamed — re-point this guard').toBeDefined();

		const writeAt = dnd!.indexOf('await api.items.update(');
		expect(writeAt, 'the item-update write moved — re-point this guard').toBeGreaterThan(-1);

		const assignAt = dnd!.indexOf('update.assigned_user_id = currentUserId');
		expect(assignAt, 'the assignee composition moved — re-point this guard').toBeGreaterThan(-1);

		// A check between composing the assignee and issuing the request.
		const between = dnd!.slice(assignAt, writeAt);
		expect(
			between,
			'the write carrying `currentUserId` is issued without re-checking the identity first, so a ' +
				"drag after an identity change PERSISTS the previous user's id as the assignee"
		).toMatch(/if \(!identityHeld\(/);
	});
});
