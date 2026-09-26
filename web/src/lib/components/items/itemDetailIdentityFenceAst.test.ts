/**
 * BUG-3084, ItemDetail — the identity GATE and the analysis AID. See
 * `src/test/identityFenceAst.ts` for what each claims and what each cannot
 * see. `itemDetailIdentityFence.test.ts` beside this keeps the site pins;
 * `itemDetailIdentityLoad.svelte.test.ts` owns the semantics on a mount.
 *
 * The machinery of both lives in `src/test/identityFenceGate.ts` (TASK-3097,
 * shared with the route surfaces); this file holds ItemDetail's TABLE.
 *
 * The GATE (`refusals`, lead ruling on checkpoint 69):
 *   - every unit the AST yields matches exactly one row below, and every row
 *     exactly one unit;
 *   - every row, every helper in HELPERS, and every IDENTIFIER_CALLBACKS row
 *     carries `reviewed`: the hash of the code it was last reviewed on
 *     (`coverage`). A mismatch refuses, naming the row and the new hash.
 *     `POPULATION_VOCABULARY` hashes the aid module's spelling lists by
 *     value, since nothing in the component covers them.
 * To accept an edit: read the row and `analysisReport` for the edited code,
 * then paste the printed hash.
 *
 * The AID (`analysisReport`): the flow analysis, which reads each row's
 * reasons.
 *   - `may` lists the commits a unit may make while no fence holds. A key is
 *     an assignment target's root name, or a call's callee text.
 *   - A row with `may` also pins the unit's exact `code`.
 *   - A row with `may` or `startSafe` pins its enclosing context (`in`).
 *
 * Round 4's mutants and the later defect fixtures still run: the gate
 * refuses each of them, and the aid still reports each of them.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { parseComponent, declarations, enumerateUnits, analyseUnit, DEFERRING_FUNCTIONS } from '../../../test/identityFenceAst';
import {
	refusals as gateRefusals,
	analysisReport as gateAnalysisReport,
	clearsBeforeFirstAwait,
	assignedTo,
	topLevelOf,
	type GateOptions,
	type GateTable,
	type Row,
	type SignedRow,
} from '../../../test/identityFenceGate';
import round4 from './itemDetailIdentityFence.round4.json';
import round6 from './itemDetailIdentityFence.round6.json';
import round7 from './itemDetailIdentityFence.round7.json';
import round8 from './itemDetailIdentityFence.round8.json';
import round9 from './itemDetailIdentityFence.round9.json';
import round10 from './itemDetailIdentityFence.round10.json';

interface GuardMutant {
	id: string;
	finding: string;
	what: string;
	old: string;
	new: string;
}
const ROUND4_MUTANTS: GuardMutant[] = round4.mutants;

const SOURCE = readFileSync(new URL('./ItemDetail.svelte', import.meta.url), 'utf8');


/**
 * Why a save-indicator settle may run after an unfenced await (BUG-3044, lead
 * ruling): it MUST run on every path, the item-switch and superseded returns
 * included, or the outstanding count pins 'saving' and every SSE gate with it.
 * What keeps a stale settle from moving the NEXT item's indicator is the
 * tracker's epoch, which `saves.reset()` bumps at load. This analysis cannot
 * see that and is deliberately not taught to trust it; the claim is pinned by
 * saveTracker.svelte.test.ts 'a settle from before a reset (item switch) is
 * ignored', which a reset that does not bump the epoch fails.
 */
const SETTLE_WHY =
	"save-indicator settle: runs on every path by design; a stale token is a no-op by the tracker's epoch (saveTracker.svelte.test.ts 'a settle from before a reset (item switch) is ignored')";

/** The same, appended to the reason of a row whose own `finally` settles. */
const SETTLE_NOTE = `; its finally settles the save indicator (${SETTLE_WHY})`;

/** The collab flush's `save` callback, exactly (`codeOf`): the settle allowance covers this code and nothing else. */
const COLLAB_SAVE_CODE =
	"async ({ ws, itemId, toSave, keepalive }) => { const genAtFlush = loadGeneration; const isForegroundCurrent = (): boolean => !keepalive && !!item && item.id === itemId && genAtFlush === loadGeneration; let saveTok: SaveToken | null = null; if (isForegroundCurrent()) { saveTok = saves.begin(); localLastSaveTime = Date.now(); // Singleton editorStore is owned by the ACTIVE side only: a FROZEN // (peeking) instance stamps its own shadow above but must not touch the // singleton (PLAN-2179 DR-2 / TASK-2181). `saveStatus` is per-instance. if (!peeking) editorStore.setLastSaveTime(Date.now()); } try { // Pass the provider's per-tab op-log cursor (TASK-1319) so the // server can advance the GC watermark when this tab is caught // up to MAX(op-log.id). When the cursor is below MAX (peer ops // not yet applied here) the server leaves the watermark // untouched — the GC sweeper must not delete rows this markdown // doesn't reflect. The cursor reflects the provider tracking // THIS item; if a navigation has already swapped to another // item the foreground flush path bails earlier on dedupe and // this code doesn't run. const opLogCursor = collabProvider && collabProvider.itemID === itemId ? collabProvider.lastOpLogID : undefined; await api.items.flushCollabContent(ws, itemId, toSave, { keepalive, opLogCursor, // BUG-3080: two teardown flushes are both dispatched before // either answers, so only the server can order them — by // this stamp. Deliberately NO expected_seq here: both would // carry the same one, and the older landing first would get // the NEWER refused. clientWrite: nextClientWrite(), }); // Post-await force_refresh check: a force_refresh frame can // arrive WHILE the PATCH is in flight (server already accepted, // items.content already overwritten — the server-side gate // covers the happens-before case where MIN had advanced past // our cursor by the time the request landed). Refuse to record // this flush as authoritative locally — returning 'skipped' // stops the flusher seeding lastFlushedContent from a // known-stale base and skips the saveStatus update. Per Codex // round 10 [P1]. if (forceRefreshInFlight) return 'skipped'; if (isForegroundCurrent()) { localLastSaveTime = Date.now(); localDirty = false; // Singleton editorStore: active side only (PLAN-2179 / TASK-2181) — // a frozen preview's remote-op flush updates only its shadow above. if (!peeking) { editorStore.setLastSaveTime(Date.now()); editorStore.setDirty(false); } if (saveTok) showSaved(saveTok); } return 'flushed'; } catch (e) { // Superseded (BUG-3080): a newer flush of this tab already // landed. Not an error, and not recorded as the last flushed // content either — this body is not what the server holds. // Classified inline rather than by a call: this catch runs after an // await with no fence of its own, and the answer is needed on // every arm, foreground or not. if (e instanceof PadApiError && e.code === 'superseded_write') return 'skipped'; if (isForegroundCurrent()) { toastStore.show('Failed to save content', 'error'); } return 'failed'; } finally { // The force_refresh and superseded returns included (BUG-3044). if (saveTok) saves.settle(saveTok); } }";

/** Top-level `async function` declarations, by name. */
const ASYNC_FUNCTIONS: Record<string, Row> = {
	adoptOrConvergeToLiveCollection: { reviewed: '5a647061f75f', why: 'myGen against loadGeneration before adopting' },
	reconcileCollectionSegment: { reviewed: 'dbb36caf0452', why: 'identityHeld after the list fetch; retags and navigates only under it' },
	jumpToSection: { reviewed: '17407aeb431b', why: 'switches this instance\'s tab and scrolls to an anchor', may: ['document.getElementById', 'document.getElementById(anchorId).scrollIntoView'] },
	ensureGraphComp: { reviewed: 'c1565cfb8a18', why: 'lazy-loads a component module into this instance', may: ['ItemGraphComp', 'graphLoadError'] },
	handleCopyRef: { reviewed: 'a5e0a0394fcc', why: 'switchedAway before the copied flag' },
	loadData: { reviewed: '7450124bf4bf', why: 'IS the load: myGen against loadGeneration after every await; the BUG-3198 re-read of a changed item runs after the install, keyed on the installed item id, and fences itself on itemGen' },
	startEditTitle: { reviewed: '17f04352d420', why: 'focuses and sizes the input it opened synchronously', may: ['el', 'titleInputEl.focus', 'titleInputEl.setSelectionRange'] },
	// Re-reviewed for BUG-2836: saveTitle's own lines are unchanged (the diff
	// touches only a $derived beside titleDraft and the textarea's aria
	// attributes), yet its hash moved; the rule below still holds.
	saveTitle: { reviewed: '38815d73d55f', why: 'gen against loadGeneration on both arms, and again after the tick that resizes a reopened editor (BUG-3115)' + SETTLE_NOTE, may: ['saves.settle'] },
	updateField: {
		reviewed: '55dd4fdc7824',
		why: 'stillCurrent() on every arm, the OCC refetch and the open-children confirm' + SETTLE_NOTE,
		may: ['saves.settle'],
		callbacks: {
			'submitOrderedOCC({send})': {
				may: ['api.items.update'],
				why: 'submitOrderedOCC calls send first with no await before it, and re-sends only after stillCurrent() with no await between (fieldWriteOrder.test.ts: "does not RE-SEND once the view moves on DURING the refetch")',
			},
			'submitOrderedOCC({refetch})': {
				may: ['api.items.get'],
				why: 'submitOrderedOCC refetches only after stillCurrent() with no await between (fieldWriteOrder.test.ts: "does not retry once the view has moved on"); a read',
			},
			// BUG-3038: re-derives a whole-list body against the refetched row.
			// Both calls are pure functions of their arguments, and the only
			// write is to `patch`, a local of this updateField call.
			'submitOrderedOCC({onRefetched})': {
				may: ['parseFields', 'rederiveListWrite'],
				why: 'pure: parseFields(latest) and rederiveListWrite(base, sent, fresh) read their arguments and return a value; the callback writes only this call\'s locals (lastServerItem, patch)',
			},
		},
	},
	flushTagSaver: {
		reviewed: '310eef4ca57e',
		why: 'identityHeld(saver.epoch) before every commit and send (each batch goes through sendTagBatch, which fences its own conflict path); the unfenced writes are to this burst\'s own identity-stamped record, and the finally deletes that record only if the registry still holds it (the get)' + SETTLE_NOTE,
		may: ['saves.settle', 'saver', 'tagSavers.get', 'tagSavers.delete'],
	},
	// BUG-3143: one tag batch, re-derived and retried on a conflict. Writes
	// only this burst's own record (confirmed, token), like flushTagSaver.
	sendTagBatch: {
		reviewed: 'e0db17122ab9',
		why: 'identityHeld(saver.epoch) before the conflict refetch and after it, so the re-send follows a check with no await between; answers null on a lost identity and the drain\'s own check stops',
		may: ['saver'],
	},
	refreshCollectionIfMoved: { reviewed: '9d45f94352dd', why: 'gen against loadGeneration after the fetch' },
	// BUG-3192 Unit B: re-reads the server's progress when the children change.
	refreshProgress: { reviewed: '88f486338d7b', why: 'gen against loadGeneration, plus the slug and workspace, after the fetch' },
	loadTagSuggestions: { reviewed: 'b03a62bf7294', why: 'identityHeld after the fetch; the identity listener re-runs it' },
	stampSourceUrl: { reviewed: 'b9d3f9d400ae', why: 'switchedAway on both arms' },
	refreshFromSource: { reviewed: '205f77aba079', why: 'switchedAway on every arm; the frozen-editor notice (BUG-2177) is after that check' },
	updateAssignedUser: { reviewed: '5e4128e45ae7', why: 'gen against loadGeneration on both arms' + SETTLE_NOTE, may: ['saves.settle'] },
	updateAgentRole: { reviewed: '9b0a6af92cc2', why: 'gen against loadGeneration on both arms' + SETTLE_NOTE, may: ['saves.settle'] },
	flushRawIfPending: {
		reviewed: 'b25da2794031',
		why: 'genAtFlush against loadGeneration after each PATCH; the re-entrancy waiter returns state; the finally clears this drain\'s own in-flight flag' + SETTLE_NOTE,
		may: ['saves.settle', 'rawFlushInFlight'],
		bareAwaits: ['await new Promise((r) => setTimeout(r, 50));'],
	},
	refreshLinksPreservingOnFailure: {
		reviewed: '38c4e090e123',
		why: 'returns a value; its callers fence. BUG-2992: records the links debt against the item id captured BEFORE the await; retryLinks re-checks item, workspace and itemGen before it commits, so a record for an item no longer on screen settles with no write',
		may: ['linksRetry.succeeded', 'linksRetry.failed'],
	},
	// BUG-2992: the links retry the helper above records. itemGen doubles as
	// the identity fence, because the identity listener's loadData bumps it.
	retryLinks: { reviewed: '721460b6df3f', why: 'item id, workspace and itemGen after the await; commits only when nothing wrote the links since' },
	// BUG-3036: the re-read an SSE event or sync pass deferred while a save was in
	// flight. The SSE item_updated shape: itemGen and the item id after each await.
	runOwedRefresh: { reviewed: '1ede6aa08b69', why: 'destroyed, itemGen and the item id after each await, as the SSE item_updated re-read it stands in for (destroyed since BUG-3198: the load path calls it too)' },
	flushCollabBeforeRestore: { reviewed: 'f9f7621c1dd2', why: 'identityHeld before its failure toast' },
	closeCopyDialog: { reviewed: '1c925f081a26', why: 'restores focus after closing synchronously', may: ['paneMenuTrigger.focus'] },
	closePushDialog: { reviewed: 'bda8c7529677', why: 'restores focus after closing synchronously', may: ['paneMenuTrigger.focus'] },
	flushContentBeforeCopy: { reviewed: 'd7083bf9954c', why: 'returns a boolean to the dialog' },
	handleCopied: { reviewed: 'f35fb099838d', why: 'switchedAway before adopting the refreshed item' },
	handleDelete: { reviewed: 'b66adcf84794', why: 'switchedAway on both arms' },
	handleRestore: { reviewed: 'c3a1732554a4', why: 'switchedAway on every arm' },
	handleDeleteLink: { reviewed: 'd147285bb264', why: 'switchedAway after each await' },
	handleCreateLink: { reviewed: 'ef19e8ce0bc8', why: 'switchedAway after each await' },
	handleMove: { reviewed: 'da5513ba767f', why: 'stillOnSource() on every arm, including inside navIfStillCurrent' },
};


/** Async functions that are not top-level declarations, in the script. */
const NESTED: SignedRow[] = [
	{ body: /event\.type === 'collection_updated'/, why: 'SSE: callbackGen after the collection fetch, itemGen on item branches; a change or a migration arriving before this load installs its item is only RECORDED (BUG-3198), synchronously, before any await', reviewed: '87c80cea46db' },
	{ body: /result\.type === 'caught_up'/, why: 'sync: callbackGen after the reconciliation, itemGen on item branches; a result arriving before this load installs its item is only RECORDED (BUG-3198), synchronously, before any await', reviewed: 'a7ae81a0da33' },
	{
		body: /flushCollabContent\(/,
		in: '(top level)',
		code: COLLAB_SAVE_CODE,
		why: 'collab save: isForegroundCurrent (genAtFlush) before UI feedback' + SETTLE_NOTE,
		reviewed: 'f197b2c9f0ef',
		may: ['saves.settle'],
	},
];

/** Async functions in the markup. */
const MARKUP: SignedRow[] = [
	{ body: /startGen/, why: 'Rich toggle: startGen against loadGeneration after each await', reviewed: '63a9cce2ed93' },
	// Re-reviewed for BUG-3050 U1 (door A4): the raw-seed refusal adds no await;
	// its post-loop editor read and the rawSeedDecision check run after the
	// existing genAtToggle fence and write only locals and a toast.
	{ body: /genAtToggle/, why: 'Markdown toggle: genAtToggle against loadGeneration after each await', reviewed: 'dea496abe16e' },
];


/** Callbacks passed to deferring calls, in the script. */
const CONTINUATIONS: SignedRow[] = [
	{
		call: /noScroll: true, \}\)\.catch\($/,
		body: /./,
		in: 'reconcileCollectionSegment',
		code: '() => { if (renameOverride === bridge) renameOverride = null; }',
		why: 'rename heal failure: clears only the bridge object this heal installed', reviewed: '9a33138e4937',
		may: ['renameOverride'],
	},
	{ call: /^setTimeout\($/, body: /copied = false/, why: 'copy-flag reset: switchedAway', reviewed: '8a8d4754017f' },
	{ call: /api\.items\.get\(wsSlug, itemSlug\)\.catch\($/, body: /./, why: 'loadData item fetch: sets a flag local to that load and re-throws', reviewed: 'f46985737ace' },
	{
		call: /^setTimeout\($/,
		body: /staleConnecting = true/,
		in: '$effect(…)',
		code: "() => { if (collabProvider?.state === 'connecting' && !hasEverSynced) { staleConnecting = true; } }",
		why: 'connection state of this instance\'s own provider', reviewed: 'b0f9f4cc5f23',
		may: ['staleConnecting'],
	},
	{ call: /\.get\(refreshCtx\.wsSlug, refreshCtx\.itemId\) \.then\($/, body: /./, why: 'force-refresh fetch: refreshGen against loadGeneration', reviewed: '85c402dd6ca9' },
	{ call: /forceRefreshNonce \+= 1; \}\) \.catch\($/, body: /./, why: 'force-refresh failure: refreshGen against loadGeneration', reviewed: 'b34c4479bb79' },
	{
		call: /^setTimeout\($/,
		body: /teardownFlushed/,
		in: 'onBeforeUnload',
		code: '() => { teardownFlushed = false; }',
		why: 're-arms the BUG-3005 teardown latch, itself identity-checked', reviewed: '05b21a3dd559',
		may: ['teardownFlushed'],
	},
	{ call: /^queueMicrotask\($/, body: /./, why: 'collab lazy seed: refuses a retired or re-identified context first', reviewed: '197a4a45d219' },
	{ call: /^tick\(\)\.then\($/, body: /./, why: 'schedules a focus frame; commits nothing itself', reviewed: 'b6f7655cb302' },
	{
		call: /^requestAnimationFrame\($/,
		body: /./,
		in: 'tick().then(…)',
		code: '() => editorInstance?.commands.focus()',
		why: 'focuses the editor after a tab switch', reviewed: 'a6e3cdf6324a',
		may: ['editorInstance.commands.focus'],
	},
	{
		call: /^setTimeout\($/,
		body: /content: toSave, client_write: nextClientWrite\(\) \}\)\.then/,
		in: 'handleContentUpdate',
		why: 'content debounce: loadData clears this timer before its first await, so the callback never runs across a load', reviewed: '6858c043b334',
		startSafe: true,
		pin: (src, unit) => clearsBeforeFirstAwait(src, 'loadData', 'contentDebounceTimer') ?? assignedTo(src, unit, 'contentDebounceTimer'),
	},
	{ call: /\{ content: toSave, client_write: nextClientWrite\(\) \}\)\.then\($/, body: /^\(\) =>/, why: 'content save: switchedAway', reviewed: '5afbf6c10e2d' },
	{ call: /showSaved\(saveTok\); \}\)\.catch\($/, body: /./, why: 'content save failure: switchedAway', reviewed: '0cb1078817c7' },
	{
		call: /\.finally\($/,
		body: /^\(\) => saves\.settle\(saveTok\)$/,
		in: 'setTimeout(…)',
		code: '() => saves.settle(saveTok)',
		why: SETTLE_WHY,
		reviewed: '08fccaaec1ae',
		may: ['saves.settle'],
	},
	{ call: /\{ keepalive: true \}\) \.then\($/, body: /./, why: 'raw keepalive save: genAtSave against loadGeneration', reviewed: 'b95a7c2a3e88' },
	{ call: /localDirty = false; \} \}\) \.catch\($/, body: /^\(\) => \{\}$/, why: 'raw keepalive failure: empty', reviewed: '2ad797bd41d1' },
	{ call: /reqItemId, \{ content: toSave, client_write: nextClientWrite\(\) \}\)\.then\($/, body: /./, why: 'raw foreground save: genAtSave against loadGeneration', reviewed: '05f885ec2567' },
	{ call: /content: item\.content, content_state: item\.content_state \}\); \} \}\)\.catch\($/, body: /./, why: 'raw foreground failure: genAtSave against loadGeneration', reviewed: '67af6367e790' },
	{
		call: /\.finally\($/,
		body: /^\(\) => saves\.settle\(saveTok\)$/,
		in: '{save}',
		code: '() => saves.settle(saveTok)',
		why: SETTLE_WHY,
		reviewed: 'd16dc96fd13d',
		may: ['saves.settle'],
	},
];

/**
 * Component-level synchronous functions a tabled unit reaches by name,
 * transitively, with the hash of the code each was last reviewed on (see
 * `coverage`). An inlined helper runs with its caller's state, so an edit to
 * one is an edit to every unit that reaches it.
 */
const HELPERS: Record<string, string> = {
	primeCanonicalSeed: '9d90b6bf0245',
	adoptCollection: 'ab38368cd8fb',
	// BUG-3050 U3: the kept local body keeps ITS content_state. Still synchronous,
	// reads only collabProvider and item, and commits only through withInflightTags.
	adoptServerItem: 'e11e0282e197',
	applyProgress: 'b28014625819',
	autoResizeTitle: '7b1e2fcd0526',
	captureIdentity: '01f30996ab05',
	handleGone: 'fafcb8c8c429',
	handleNavigateAway: '44ad4f59673b',
	identityHeld: '1c5505d51f73',
	navigateToCollectionRoot: '64dce75693d3',
	refreshPrintMeta: '2312cc481ca5',
	runTeardownFlush: '6c7e2a216f4f',
	// BUG-3124 unit B: the cursor-advance settle. Synchronous; arms the flusher's
	// single timer only for the current, identity-held context.
	showSaved: '456dd972dcf5',
	settleCollabIfCurrent: '438f4aed6d2f',
	// BUG-2367: the move's success toast, naming any not_unique drop.
	// Synchronous; reads only its arguments and writes only the toast store.
	showMovedToast: '6a32f06fbd9f',
	switchedAway: '8ba85844f406',
	// BUG-3036: refuses a snapshot of the shown item whose seq is strictly older.
	withInflightTags: 'd9aeb96e6bd1',
	// BUG-3198: records the ids a delivery named while no item was installed.
	// Synchronous; writes only the plain `changedDuringLoad` record.
	noteChangeDuringLoad: 'ed3955c8ecae',
	// BUG-3198: a fresh, empty record. Pure.
	emptyChangedDuringLoad: 'd94fd85a8f1f',
};

/**
 * Deferring calls whose callback is not a function literal. `count` pins the
 * call text itself; `reviewed` hashes the component-level statements that
 * DECLARE or REBIND the names these calls pass, because such a call site may
 * sit outside every hashed unit (both `.then(ensureGraphComp)` sites do: one
 * in an `$effect`, one in `setGraphParam`), and then nothing else covers a
 * rebinding of what it defers — round 9 F4.
 */
const IDENTIFIER_CALLBACKS: GateTable['identifierCallbacks'] = [
	{ text: 'Promise.resolve().then(ensureGraphComp)', count: 2, reviewed: '6cdb881b83f0', why: 'ensureGraphComp is itself a unit in the table above' },
	{ text: 'setTimeout(r, 50)', count: 1, reviewed: 'f7a2eb3b36e7', why: 'resolves flushRawIfPending\'s re-entrancy waiter' },
];

const TABLE: GateTable = {
	asyncFunctions: ASYNC_FUNCTIONS,
	nested: NESTED,
	markup: MARKUP,
	continuations: CONTINUATIONS,
	helpers: HELPERS,
	identifierCallbacks: IDENTIFIER_CALLBACKS,
};

/** THE GATE on ItemDetail's table (see `identityFenceGate.ts`). */
const refusals = (code: string, opts: GateOptions = {}) => gateRefusals(TABLE, code, opts);

/** THE AID on ItemDetail's table; required quiet on the committed component. */
const analysisReport = (code: string, extraNested: SignedRow[] = []) => gateAnalysisReport(TABLE, code, extraNested);


describe('ItemDetail: every async unit is tabled, and none commits past an unfenced await (AST)', () => {
	it('the component as written passes the gate', () => {
		expect(refusals(SOURCE)).toEqual([]);
	});

	it('the aid is quiet on the component as written, so a re-reader starts from an empty report', () => {
		expect(analysisReport(SOURCE)).toEqual([]);
	});

	it('every row carries the hash it was reviewed on, and the helper population is not empty', () => {
		for (const r of [...Object.values(ASYNC_FUNCTIONS), ...NESTED, ...MARKUP, ...CONTINUATIONS]) {
			expect(r.reviewed, `row (${r.why}) has no reviewed hash`).toMatch(/^[0-9a-f]{12}$/);
		}
		// The fences every unit leans on are inlined helpers, so they are hashed.
		for (const h of ['identityHeld', 'switchedAway', 'captureIdentity', 'showSaved']) expect(HELPERS[h], `${h} is not in HELPERS`).toBeDefined();
	});

	it('a row that may commit or starts safe is pinned to its place, and a row that may commit to its code', () => {
		for (const r of [...NESTED, ...MARKUP, ...CONTINUATIONS]) {
			if (r.may || r.startSafe) expect(r.in, `row (${r.why}) has no \`in\``).toBeDefined();
			if (r.may) expect(r.code, `row (${r.why}) has no \`code\``).toBeDefined();
		}
	});

	it('the analysis is not vacuous: it finds units of every kind, and it reads the fences it relies on', () => {
		const src = parseComponent(SOURCE);
		const { units } = enumerateUnits(src);
		expect(units.filter((u) => u.kind === 'async-function' && u.name).length).toBe(Object.keys(ASYNC_FUNCTIONS).length);
		expect(units.filter((u) => u.kind === 'continuation').length).toBe(CONTINUATIONS.length);
		const decls = declarations(src);
		for (const helper of ['switchedAway', 'stillCurrent', 'stillOnSource', 'isForegroundCurrent']) {
			expect(decls.fences.has(helper), `${helper} is no longer read as a fence`).toBe(true);
		}
		for (const capture of ['gen', 'myGen', 'callbackGen', 'genAtFlush', 'identity']) {
			expect(decls.captures.has(capture), `${capture} is no longer read as a capture`).toBe(true);
		}
	});
});

/** The sync handler's list lookup, with its element renamed to `item`. */
const SYNC_FIND_AS_ITEM: [string, string] = [
	'\t\t\t\tconst updated = result.changes.updated.find(i => i.id === reqItemId);\n',
	'\t\t\t\tconst updated = result.changes.updated.find((item) => item.id === reqItemId);\n',
];
/** The sync handler's full-refresh fallback: fetch, fence, adopt. */
const SYNC_REFRESH_FENCE = '\t\t\t\tif (!item || item.id !== reqItemId || myItemGen !== itemGen) return;\n';
const SYNC_REFRESH_FENCE_AND_ADOPT = SYNC_REFRESH_FENCE + '\t\t\t\titem = adoptServerItem(updated);\n';
const SYNC_REFRESH_FENCED = '\t\t\t\tconst updated = await api.items.get(reqWsSlug, reqItemSlug);\n' + SYNC_REFRESH_FENCE_AND_ADOPT;

/** loadData's unconditional debounce clear, which the debounce row's pin relies on. */
const LOAD_DATA_CLEAR = '\t\tclearTimeout(contentDebounceTimer);\n\t\tcontentDebounceTimer = undefined;\n\t\tcollabFlusher.cancel();\n';

/** saveTitle's capture, made reassignable. */
const TITLE_GEN_LET: [string, string] = [
	"\t\tconst gen = loadGeneration;\n\t\tconst saveTok = saves.begin();\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
	"\t\tlet gen = loadGeneration;\n\t\tconst saveTok = saves.begin();\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
];

/** saveTitle records an identity stamp at entry, before its await. */
const TITLE_STAMP_AT_ENTRY: [string, string] = [
	"\t\tconst gen = loadGeneration;\n\t\tconst saveTok = saves.begin();\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
	"\t\tconst gen = loadGeneration;\n\t\tconst snap = { epoch: captureIdentity() };\n\t\tconst saveTok = saves.begin();\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
];

/** saveTitle's fence and the two commits under it: the anchor most round-5 edits rewrite. */
const TITLE_FENCE_AND_COMMITS =
	"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n";

describe('ItemDetail AST guard: round 4\'s edits are all refused (lead ruling, condition 1)', () => {
	// Without a clean baseline every mutant is "refused" for free.
	const BASELINE = refusals(SOURCE);

	it.each(ROUND4_MUTANTS.map((m) => [m.id, m] as const))('%s is refused', (_id, m) => {
		expect(BASELINE, 'the unmutated component is refused, so no mutant result means anything').toEqual([]);
		expect(SOURCE.split(m.old).length - 1, `${m.id}'s anchor is not in the component exactly once — re-point the fixture`).toBe(1);
		const mutated = SOURCE.replace(m.old, m.new);
		expect(refusals(mutated), `${m.id} (${m.finding}: ${m.what}) passed the guard`).not.toEqual([]);
	});

	/**
	 * Defects found in the ANALYSIS itself by later review rounds. Each is an
	 * edit the analysis accepted when it should not have. `refuses` is the
	 * refusal the edit must produce — every string in one line — so an entry
	 * cannot pass because some OTHER part of the edit happens to be refused
	 * (round 5's E5 was refused only through an inlined helper's call, while
	 * the `item =` write it was about went unreported).
	 */
	const ANALYSIS_DEFECTS: Array<{ id: string; subs: Array<[string, string]>; refuses: string[] }> = [
		{
			// Round 5 on #1387: `?:` was walked as `consequent && alternate`, so an
			// unsafe consequent short-circuited the walk and the alternate's commit
			// was never seen.
			id: 'R5-1 a commit in the alternate of a conditional whose consequent awaits',
			subs: [[
				"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n",
				"{ title: titleDraft.trim() });\n\t\t\tupdated ? await tick() : (item = withInflightTags(updated));\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n",
			]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		// Round 5 P1-1: a fence to the LEFT of an await in one test expression was
		// read as holding after that await.
		{
			id: 'R5 E1 a fence before an await in one && test',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen === loadGeneration && (await dialogs.confirm('Keep the new title?'))) {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t\tshowSaved(saveTok);\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E1b a fence boolean whose initialiser awaits after the fence',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst keep = gen === loadGeneration && (await dialogs.confirm('Keep the new title?'));\n\t\t\tif (!keep) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E1c the early-return form: fence, then an await, in one || test',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id || !(await dialogs.confirm('Keep the new title?'))) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		// Round 5 P1-2: a callback handed to anything but the deferring names was
		// walked as if it ran inside the call, and a function-valued property was
		// not walked at all.
		{
			id: 'R5 E6 a commit in the onRefetched property, which runs after the refetch await',
			subs: [["\t\t\t\t\tlastServerItem = latest;\n", "\t\t\t\t\tlastServerItem = latest;\n\t\t\t\t\titem = withInflightTags(latest);\n"]],
			refuses: ['updateField()', 'callback submitOrderedOCC({onRefetched})', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E7 updateField\'s open-children confirm callback loses its stillCurrent fence',
			subs: [["\t\t\t\t\t\tif (!stillCurrent()) throw new Error('switched away');\n", '']],
			refuses: ['updateField()', 'callback confirmOpenChildrenOrThrow(…)', 'calls submitOrderedOCC after an unfenced await'],
		},
		{
			id: 'R5 E7b handleMove\'s open-children confirm callback loses its stillOnSource fence',
			subs: [["\t\t\t\t\t\tif (!stillOnSource()) throw new Error('switched away');\n", '']],
			refuses: ['handleMove()', 'callback confirmOpenChildrenOrThrow(…)', 'after an unfenced await'],
		},
		{
			id: 'R5 E8 a commit inside a requestIdleCallback callback',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\trequestIdleCallback(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t\tshowSaved(saveTok);\n\t\t\t});\n"]],
			refuses: ['saveTitle()', 'callback requestIdleCallback(…)', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E8b a commit inside an event listener',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\twindow.addEventListener('focus', () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t}, { once: true });\n"]],
			refuses: ['saveTitle()', 'callback window.addEventListener(…)', 'assigns item after an unfenced await'],
		},
		{
			// A helper handed over by NAME runs as late as a literal would.
			id: 'R5 P1-2 a commit in a helper passed by name to an event listener',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst applyTitleLater = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\twindow.addEventListener('focus', applyTitleLater);\n"]],
			refuses: ['saveTitle()', 'callback window.addEventListener(…)', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P1-2 a commit in a helper passed by name as an object property',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst applyTitleLater = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\tdialogs.register({ onClose: applyTitleLater });\n"]],
			refuses: ['saveTitle()', 'callback dialogs.register({onClose})', 'assigns item after an unfenced await'],
		},
		{
			// A function value that escapes by any route runs whenever its holder
			// calls it.
			id: 'R5 P1-2 a helper stored by name into component state',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst applyTitleLater = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\trenameOverride = applyTitleLater;\n"]],
			refuses: ['saveTitle()', 'callback applyTitleLater', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P1-2 a function literal stored into component state',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\trenameOverride = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n"]],
			refuses: ['saveTitle()', 'callback renameOverride =', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P1-2 a function declared under a name used twice, passed by name',
			subs: [
				[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst onTitleClose = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\twindow.addEventListener('blur', onTitleClose);\n"],
				['\tfunction showSaved(saveTok: SaveToken) {\n', '\tfunction showSaved(saveTok: SaveToken) {\n\t\tconst onTitleClose = () => {};\n\t\tvoid onTitleClose;\n'],
			],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// A literal in a callee runs now; a literal in an ARGUMENT inside that
			// callee does not.
			id: 'R5 P1-2 a literal passed as an argument inside a called expression',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\t(dialogs.register(updated ? () => { item = withInflightTags(updated); } : null) ?? tick)();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// Only an empty Set/Map is an allowed constructor default (lead ruling on
			// checkpoint 63, condition 3).
			id: 'R6 a default value that constructs something with effects',
			subs: [['\tfunction showSaved(saveTok: SaveToken) {\n', "\tfunction showSaved(stream = new TitleStream()) {\n\t\tvoid stream;\n"]],
			refuses: ['default value calls or assigns'],
		},
		{
			// A fence boolean stops being one when it is written (round 6 ruling B,
			// class 6); here the write, not an await, is what makes it stale.
			id: 'R6 a fence boolean overwritten after it is computed',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tlet titleOk = gen === loadGeneration;\n\t\t\ttitleOk = true;\n\t\t\tif (!titleOk) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// H12b with the operands the other way round.
			id: 'R6 loadGeneration compared against an identity capture',
			subs: [
				[TITLE_GEN_LET[0], TITLE_GEN_LET[0].replace('\t\tsaveStatus', '\t\tconst identity = captureIdentity();\n\t\tsaveStatus')],
				[TITLE_FENCE_AND_COMMITS, '{ title: titleDraft.trim() });\n\t\t\tif (loadGeneration !== identity) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n'],
			],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// Round 6 class 9: the clear must be reached on every path to the
			// first await. An early return before it skips it on one path.
			id: 'R6 loadData can return before it clears the debounce',
			subs: [[LOAD_DATA_CLEAR, '\t\tif (!item) return;\n' + LOAD_DATA_CLEAR]],
			refuses: ['loadData no longer clears contentDebounceTimer before its first await'],
		},
		{
			id: 'R6 loadData awaits before it clears the debounce',
			subs: [[LOAD_DATA_CLEAR, '\t\tawait tick();\n' + LOAD_DATA_CLEAR]],
			refuses: ['loadData no longer clears contentDebounceTimer before its first await'],
		},
		{
			// Iteration methods count as synchronous only on a receiver the unit
			// declared; anything else could be an object whose `forEach` stores
			// the callback.
			id: 'R5 P1-2 an iteration-named method on a receiver the unit did not declare',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tsseService.forEach(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t});\n"]],
			refuses: ['saveTitle()', 'callback sseService.forEach(…)', 'assigns item after an unfenced await'],
		},
		{
			// A row's `may` covers the unit's own statements, not the callbacks it
			// creates: flushTagSaver may delete its registry entry, a listener it
			// registers may not.
			id: "R5 P1-2 a row's may does not reach a callback the unit creates",
			subs: [["\tasync function flushTagSaver(saver: TagSaver) {\n\t\tsaver.running = true;\n", "\tasync function flushTagSaver(saver: TagSaver) {\n\t\twindow.addEventListener('focus', () => tagSavers.delete(saver.itemId));\n\t\tsaver.running = true;\n"]],
			refuses: ['flushTagSaver()', 'callback window.addEventListener(…)', 'calls tagSavers.delete after an unfenced await'],
		},
		{
			// Round 5 P1-4: the debounce row starts safe because loadData clears
			// contentDebounceTimer; the pin must also prove this callback IS that
			// timer's.
			id: 'R5 E12 the debounce callback moves onto a timer loadData does not clear',
			subs: [['\t\tcontentDebounceTimer = setTimeout(() => {\n', '\t\tlegacyContentTimer = setTimeout(() => {\n']],
			refuses: ['starts safe, but its setTimeout is not assigned to contentDebounceTimer'],
		},
		// Round 5 P2-1..P2-3: loop and switch flow.
		{
			id: 'R5 E2 a commit in a for-await body',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tfor await (const t of api.items.titleStream(wsSlug, targetItem.id)) {\n\t\t\t\ttitleDraft = t;\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns titleDraft after an unfenced await'],
		},
		{
			id: 'R5 P2-1 a commit after a for-await loop',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tfor await (const t of api.items.titleStream(wsSlug, targetItem.id)) void t;\n\t\t\ttitleDraft = '';\n"]],
			refuses: ['saveTitle()', "assigns titleDraft after an unfenced await — titleDraft = ''"],
		},
		{
			id: 'R5 E3 a commit after a for-of over an awaited list whose body always returns',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tfor (const newer of await api.items.newerVersions(wsSlug, targetItem.id)) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P2-2 a commit after a while whose test awaits and whose body always returns',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\twhile (await api.items.hasNewer(wsSlug, targetItem.id)) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E4 a commit in the case whose test awaits',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tswitch (titleDraft) {\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\ttitleDraft = '';\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns titleDraft after an unfenced await'],
		},
		{
			id: 'R5 P2-3 a commit in the default case, reached only after an awaiting test',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tswitch (titleDraft) {\n\t\t\t\tdefault:\n\t\t\t\t\ttitleDraft = '';\n\t\t\t\t\tbreak;\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\tbreak;\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns titleDraft after an unfenced await'],
		},
		{
			id: 'R5 P2-3 a commit after a switch with no default whose test awaits',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tswitch (titleDraft) {\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\treturn;\n\t\t\t}\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		// Round 5 P2-4: an identity comparison between two STAMPS proves nothing.
		{
			id: 'R5 E9 flushTagSaver compares two stale identity stamps',
			subs: [['\t\t\t\tif (!identityHeld(saver.epoch)) {\n', '\t\t\t\tif (tagSavers.get(saver.itemId)!.epoch !== saver.epoch) {\n']],
			refuses: ['flushTagSaver()', 'after an unfenced await'],
		},
		{
			id: 'R5 P2-4 flushTagSaver compares two live identity reads',
			subs: [['\t\t\t\tif (!identityHeld(saver.epoch)) {\n', '\t\t\t\tif (captureIdentity() !== authStore.identityEpoch) {\n']],
			refuses: ['flushTagSaver()', 'after an unfenced await'],
		},
		{
			id: 'R5 P2-4 flushTagSaver compares a live identity read against something that is not a stamp',
			subs: [['\t\t\t\tif (!identityHeld(saver.epoch)) {\n', '\t\t\t\tif (authStore.identityEpoch !== saver.itemId.length) {\n']],
			refuses: ['flushTagSaver()', 'after an unfenced await'],
		},
		// Round 5 P2-5: a capture refreshed after an await makes every later check
		// against it pass, whatever the refresh is spelled as.
		{
			id: 'R5 E10 saveTitle refreshes its capture through a cast',
			subs: [TITLE_GEN_LET, ['{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration', '{ title: titleDraft.trim() });\n\t\t\tgen = loadGeneration as number;\n\t\t\tif (gen !== loadGeneration']],
			refuses: ['saveTitle()', 'rewrites capture gen after an unfenced await'],
		},
		{
			id: 'R5 E10b saveTitle refreshes its capture through ??',
			subs: [TITLE_GEN_LET, ['{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration', '{ title: titleDraft.trim() });\n\t\t\tgen = loadGeneration ?? gen;\n\t\t\tif (gen !== loadGeneration']],
			refuses: ['saveTitle()', 'rewrites capture gen after an unfenced await'],
		},
		{
			id: 'R5 P2-5 saveTitle bumps its capture',
			subs: [TITLE_GEN_LET, ['{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration', '{ title: titleDraft.trim() });\n\t\t\tgen++;\n\t\t\tif (gen !== loadGeneration']],
			refuses: ['saveTitle()', 'rewrites capture gen after an unfenced await'],
		},
		// Round 5 P2-6: every param of every function in or around the unit was a
		// capture, so any of them compared to loadGeneration was a fence.
		{
			id: "R5 E11 updateField compares the onRefetched param to loadGeneration",
			subs: [['\t\t\tconst fresh = await submitWithOCC(false);\n\t\t\tif (!stillCurrent()) return;\n', '\t\t\tconst fresh = await submitWithOCC(false);\n\t\t\tif (latest !== loadGeneration) return;\n']],
			refuses: ['updateField()', 'assigns item after an unfenced await'],
		},
		// Round 5 P2-7 / P2-8: a row's allowance covered more than its reason, and
		// a row could re-point by a move.
		{
			id: 'R5 E13 the rename heal clears the override without its bridge compare',
			subs: [['\t\t\t\tif (renameOverride === bridge) renameOverride = null;\n', '\t\t\t\trenameOverride = null;\n']],
			refuses: ['matches 0 table rows'],
		},
		{
			id: "R5 P2-8 the rename heal's catch moves onto the SSE rename navigation",
			subs: [
				[
					"\t\t\t\tnoScroll: true,\n\t\t\t}).catch(() => {\n\t\t\t\t// A failed/cancelled navigation must not leave the override\n\t\t\t\t// bridging to a URL we never reached. Identity compare (this\n\t\t\t\t// heal's own bridge object, round 4 P2) so a cancelled older\n\t\t\t\t// navigation can't clear a newer bridge to the same slug.\n\t\t\t\tif (renameOverride === bridge) renameOverride = null;\n\t\t\t});\n",
					'\t\t\t\tnoScroll: true,\n\t\t\t});\n',
				],
				[
					'\t\t\t\t\t\t\tnoScroll: true,\n\t\t\t\t\t\t});\n',
					'\t\t\t\t\t\t\tnoScroll: true,\n\t\t\t\t\t\t}).catch(() => {\n\t\t\t\t\t\t\tif (renameOverride === bridge) renameOverride = null;\n\t\t\t\t\t\t});\n',
				],
			],
			refuses: ['matches 0 table rows'],
		},
		{
			// A callback allowance covers what its reason covers: the refetch may
			// read, not commit.
			id: 'R5 P1-2 the refetch callback commits beyond its allowance',
			subs: [["\t\t\t\trefetch: () => api.items.get(targetWs, targetItem.id),\n", "\t\t\t\trefetch: () => ((saveStatus = 'saving'), api.items.get(targetWs, targetItem.id)),\n"]],
			refuses: ['updateField()', 'callback submitOrderedOCC({refetch})', 'assigns saveStatus after an unfenced await'],
		},
		// Round 5 P1-3: every binding anywhere in a unit, nested functions
		// included, made that name local for the whole unit.
		{
			id: 'R5 E5 a nested arrow param named item excuses an unfenced item write (adopting)',
			subs: [SYNC_FIND_AS_ITEM, [SYNC_REFRESH_FENCED, SYNC_REFRESH_FENCED.replace(SYNC_REFRESH_FENCE_AND_ADOPT, '\t\t\t\titem = adoptServerItem(updated);\n' + SYNC_REFRESH_FENCE)]],
			refuses: ['assigns item after an unfenced await — item = adoptServerItem(updated)'],
		},
		{
			id: 'R5 E5b a nested arrow param named item excuses an unfenced item write (verbatim)',
			subs: [SYNC_FIND_AS_ITEM, [SYNC_REFRESH_FENCED, SYNC_REFRESH_FENCED.replace(SYNC_REFRESH_FENCE_AND_ADOPT, '\t\t\t\titem = updated;\n' + SYNC_REFRESH_FENCE)]],
			refuses: ['assigns item after an unfenced await — item = updated'],
		},
		{
			// A block's bindings hold only inside that block: a `const item` in
			// one branch excuses no `item =` outside it.
			id: 'R5 P1-3 a block-scoped declaration excuses a write outside its block',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (updated) {\n\t\t\t\tconst item = updated;\n\t\t\t\tvoid item;\n\t\t\t}\n\t\t\titem = withInflightTags(updated);\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved(saveTok);\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await — item = withInflightTags(updated)'],
		},
		{
			// An inlined helper resolves names in ITS scopes: a caller's local that
			// shares a name with the state the helper writes excuses nothing.
			id: "R5 P1-3 a caller's local does not excuse the same name inside an inlined helper",
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst saveStatus = updated.title;\n\t\t\tshowSaved(saveTok);\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n"]],
			refuses: ['saveTitle()', 'showSaved() -> calls saves.succeed after an unfenced await'],
		},
	];
	it('a callback allowance that names no callback the unit creates is refused', () => {
		const src = parseComponent(SOURCE);
		const unit = enumerateUnits(src).units.find((u) => u.name === 'updateField')!;
		expect(() =>
			analyseUnit(src, declarations(src), unit, { startSafe: true, callbacks: new Map([['submitOrderedOCC({sendd})', new Set(['api.items.update'])]]) })
		).toThrow(/names no callback this unit creates/);
	});

	/**
	 * Round 6 on #1387: edits the guard accepted at 31f3b5e9, from the
	 * reviewer's probe (checkpoint 63), each tagged with its class. Every one
	 * is either closed here or listed in KNOWN_GAPS below.
	 */
	const ROUND6 = (round6.mutants as Array<{ id: string; class: number; subs: string[][]; refuses: string[]; gate?: boolean }>).map((m) => ({
		id: `${m.id} (class ${m.class})`,
		subs: m.subs as Array<[string, string]>,
		refuses: m.refuses,
		// Class 8 was the first hash; its refusal is the gate's now.
		gate: m.gate,
	}));

	/**
	 * Round 7 on #1387 (checkpoint 67): edits the guard accepted at 6e1ba4af.
	 * An F1 edit arrives with the NESTED row the guard asks for, since without
	 * the row it is refused for being untabled rather than for what it commits.
	 */
	interface Round7Nested {
		body: string;
		why: string;
		callbacks?: Record<string, { may: string[]; why: string }>;
	}
	const ROUND7 = (round7.mutants as Array<{ id: string; class: string; subs: string[][]; nested?: Round7Nested[]; refuses: string[] }>).map((m) => ({
		id: `${m.id} (class ${m.class})`,
		subs: m.subs as Array<[string, string]>,
		nested: (m.nested ?? []).map((r): SignedRow => ({ ...r, body: new RegExp(r.body) })),
		refuses: m.refuses,
	}));

	const applySubs = (subs: Array<[string, string]>) => {
		let code = SOURCE;
		for (const [from, to] of subs) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		return code;
	};

	const DEFECTS: Array<{ id: string; subs: Array<[string, string]>; nested?: SignedRow[]; refuses: string[]; gate?: boolean }> = [...ANALYSIS_DEFECTS, ...ROUND6, ...ROUND7];
	it.each(DEFECTS.map((d) => [d.id, d] as const))('analysis defect stays closed: %s', (_id, d) => {
		expect(BASELINE).toEqual([]);
		const code = applySubs(d.subs);
		// The gate refuses every one of these: each edits hashed code.
		expect(refusals(code, { extraNested: d.nested }), 'the gate accepted an edit to a fenced unit').not.toEqual([]);
		// The aid still reports it, which is what these fixtures regress.
		const got = d.gate ? refusals(code) : analysisReport(code, d.nested ?? []);
		expect(
			got.some((line) => d.refuses.every((s) => line.includes(s))),
			`no refusal carries ${JSON.stringify(d.refuses)}; got ${JSON.stringify(got, null, 1)}`
		).toBe(true);
	});

	/**
	 * AID GAPS (checkpoint 63 condition 4; checkpoint 69 condition 4): edits
	 * the ANALYSIS accepts although they reintroduce a stale commit, each with
	 * the sentence that says why. Each is listed in the "does not see" part of
	 * `identityFenceAst.ts`'s header. The gate refuses every one of them by a
	 * hash mismatch. When the aid starts reporting one, move it to
	 * ANALYSIS_DEFECTS.
	 */
	const KNOWN_GAPS: Array<{ id: string; model: string; subs: Array<[string, string]> }> = [
		{
			id: 'a non-array object aliased to a local, whose forEach defers its callback',
			model:
				'trusted synchronous callee: iteration methods on a local receiver are taken to be array methods; binding a service to a local to call a deferring method named forEach is evasion, not an ordinary edit',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + '\t\t\tconst later = sseService;\n\t\t\tlater.forEach(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t});\n']],
		},
		{
			id: 'a stamp minted after the await and compared at once',
			model:
				'contrived fence: a stamp is trusted to have been recorded earlier; building one from a live read right before comparing it to a live read is a fence that cannot fail, which no author trying to comply writes',
			subs: [[TITLE_FENCE_AND_COMMITS, '{ title: titleDraft.trim() });\n\t\t\tconst fresh = { epoch: captureIdentity() };\n\t\t\tif (authStore.identityEpoch !== fresh.epoch) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n']],
		},
		// Round 7 (checkpoint 67): the reviewer's out-of-model findings.
		...(round7.gaps as Array<{ id: string; class: string; model: string; subs: string[][] }>).map((g) => ({
			id: `${g.id} (class ${g.class})`,
			model: g.model,
			subs: g.subs as Array<[string, string]>,
		})),
		// Round 8 (checkpoint 69): recorded, not fixed, under the lead's ruling.
		...(round8.aidGaps as Array<{ id: string; model: string; subs: string[][] }>).map((g) => ({
			id: `R8 ${g.id}`,
			model: g.model,
			subs: g.subs as Array<[string, string]>,
		})),
	];
	it.each(KNOWN_GAPS.map((g) => [g.id, g] as const))('aid gap, which the gate refuses: %s', (_id, g) => {
		expect(BASELINE).toEqual([]);
		const code = applySubs(g.subs);
		expect(analysisReport(code), 'the aid now reports this: move it to ANALYSIS_DEFECTS').toEqual([]);
		expect(
			refusals(code).some((l) => l.includes('has changed since')),
			'the gate does not refuse this aid gap by a hash mismatch'
		).toBe(true);
	});

	/**
	 * Controls for the AID: an analysis that reports everything also reports
	 * every mutant. These edits commit nothing, and the aid must stay quiet on
	 * them. (The gate refuses them all, since each edits a hashed function; its
	 * own controls are at the end of this file.)
	 */
	const ACCEPTED: Array<{ id: string; old: string; new: string; also?: Array<[string, string]> }> = [
		{
			id: 'a comment after an await, before the fence',
			old: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n",
			new: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n\t\t\t// a note\n",
		},
		{
			id: 'a pure local computed after an await, before the fence',
			old: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n",
			new: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n\t\t\tconst snapshot = JSON.stringify(updated);\n",
		},
		{
			// A helper's own params are captures inside its own body, whatever
			// they are called: switchedAway stays a fence with its param renamed.
			id: "switchedAway's generation param renamed",
			old: '\tfunction switchedAway(targetItem: Item, gen: number): boolean {\n\t\treturn gen !== loadGeneration || item?.id !== targetItem.id;\n',
			new: '\tfunction switchedAway(targetItem: Item, expected: number): boolean {\n\t\treturn expected !== loadGeneration || item?.id !== targetItem.id;\n',
		},
		{
			// A catch param is a local of the handler, even though the handler is
			// entered after an await.
			id: 'a write to the catch param in an unfenced handler',
			old: "\t\t} catch (err: any) {\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n",
			new: "\t\t} catch (err: any) {\n\t\t\terr = null;\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n",
		},
		{
			// An inlined helper declared inside a block sees that block's names.
			id: "a helper writing a local of the block it is declared in",
			old: '{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration',
			new: '{ title: titleDraft.trim() });\n\t\t\tlet bumped = 0;\n\t\t\tconst bumpTitleCount = () => {\n\t\t\t\tbumped = 1;\n\t\t\t};\n\t\t\tbumpTitleCount();\n\t\t\tif (gen !== loadGeneration',
		},
		{
			// A loop head's names are locals of the loop, after an await too.
			id: 'a write to a loop-head binding after an await',
			old: TITLE_FENCE_AND_COMMITS,
			new: TITLE_FENCE_AND_COMMITS + '\t\t\tfor (let attempt = 0; attempt < 1; attempt++) {\n\t\t\t\tawait tick();\n\t\t\t\tattempt = 2;\n\t\t\t}\n',
		},
		{
			id: 'a commit moved BELOW its fence stays accepted',
			old: "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n",
			new: "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved(saveTok);\n\t\t\titem = withInflightTags(updated);\n",
		},
		{
			// A stamp rooted in a local of the unit is a stamp inside a fence
			// boolean's initialiser too (round 7 F4: the initialiser is read in its
			// own scope).
			id: 'a fence boolean over a stamp the unit recorded before its await',
			old: TITLE_STAMP_AT_ENTRY[0],
			new: TITLE_STAMP_AT_ENTRY[1],
			also: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst held = authStore.identityEpoch === snap.epoch;\n\t\t\tif (!held || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
		},
		{
			// ...and inside a helper's body, read in the helper's scope.
			id: 'a fence helper over a stamp the unit recorded before its await',
			old: TITLE_STAMP_AT_ENTRY[0],
			new: TITLE_STAMP_AT_ENTRY[1].replace('\t\tconst saveTok', '\t\tconst snapHeld = () => authStore.identityEpoch === snap.epoch;\n\t\tconst saveTok'),
			also: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (!snapHeld() || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"]],
		},
	];
	it.each(ACCEPTED.map((c) => [c.id, c] as const))('control accepted: %s', (_id, c) => {
		expect(BASELINE).toEqual([]);
		expect(analysisReport(applySubs([[c.old, c.new], ...(c.also ?? [])]))).toEqual([]);
	});
});

describe('ItemDetail identity gate: a fenced unit cannot change without a re-read (lead ruling on checkpoint 69)', () => {
	const GATE_BASELINE = refusals(SOURCE);
	const changedHashes = (lines: string[]) => new Set(lines.flatMap((l) => (l.includes('has changed since') ? [l.match(/code ([0-9a-f]{12})/)![1]!] : [])));

	const ROUND8 = round8.gate as Array<{ id: string; subs: string[][]; refuses: string[] }>;
	it.each(ROUND8.map((g) => [g.id, g] as const))('round 8 edit refused by a hash mismatch: %s', (_id, g) => {
		expect(GATE_BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of g.subs as Array<[string, string]>) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		const got = refusals(code);
		expect(got.some((l) => g.refuses.every((s) => l.includes(s))), `no refusal carries ${JSON.stringify(g.refuses)}; got ${JSON.stringify(got, null, 1)}`).toBe(true);
	});

	const ROUND9 = round9.gate as Array<{ id: string; subs: string[][]; refuses: string[] }>;
	it.each(ROUND9.map((g) => [g.id, g] as const))('round 9 edit refused by a hash mismatch: %s', (_id, g) => {
		expect(GATE_BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of g.subs as Array<[string, string]>) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		const got = refusals(code);
		expect(got.some((l) => g.refuses.every((s) => l.includes(s))), `no refusal carries ${JSON.stringify(g.refuses)}; got ${JSON.stringify(got, null, 1)}`).toBe(true);
	});

	const ROUND10 = round10.gate as Array<{ id: string; subs: string[][]; refuses: string[] }>;
	it.each(ROUND10.map((g) => [g.id, g] as const))('round 10 edit refused by a hash mismatch: %s', (_id, g) => {
		expect(GATE_BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of g.subs as Array<[string, string]>) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		const got = refusals(code);
		expect(got.some((l) => g.refuses.every((s) => l.includes(s))), `no refusal carries ${JSON.stringify(g.refuses)}; got ${JSON.stringify(got, null, 1)}`).toBe(true);
	});

	/**
	 * GATE GAPS: edits the gate accepts, each outside what it hashes, with the
	 * sentence that says why. When one starts being refused, it has closed.
	 */
	const GATE_GAPS = [
		...(round8.gateGaps as Array<{ id: string; model: string; subs: string[][] }>),
		...(round9.gateGaps as Array<{ id: string; model: string; subs: string[][] }>),
	];
	it.each(GATE_GAPS.map((g) => [g.id, g] as const))('gate gap, still outside what is hashed: %s', (_id, g) => {
		expect(GATE_BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of g.subs as Array<[string, string]>) {
			expect(code.split(from).length - 1).toBe(1);
			code = code.replace(from, to);
		}
		expect(refusals(code), 'this gap is now refused: move it to the round 8 gate fixtures').toEqual([]);
	});

	it('shrinking the population vocabulary is refused, although no row changes (round 9, smaller notes)', () => {
		// The population is a spelling list in the AID module, and the component
		// cannot cover it: drop `setTimeout` from it and every setTimeout unit
		// leaves the population, taking its row's claim with it. Mutating the
		// imported Set is the only honest fixture — the alternative is scanning
		// that file's source, which is what this whole guard stopped doing.
		expect(GATE_BASELINE).toEqual([]);
		expect(DEFERRING_FUNCTIONS.has('setTimeout')).toBe(true);
		DEFERRING_FUNCTIONS.delete('setTimeout');
		try {
			expect(
				refusals(SOURCE).some((l) => l.includes('the population vocabulary') && l.includes('has changed since it was reviewed'))
			).toBe(true);
		} finally {
			DEFERRING_FUNCTIONS.add('setTimeout');
		}
		expect(refusals(SOURCE), 'the vocabulary was not restored').toEqual([]);
	});

	it('a concise body wrapped in braces is refused, and so is a change of declaration form (round 11 finding 1)', () => {
		// `() => EXPR` RETURNS the expression; `() => { EXPR }` returns undefined.
		// Neither the braces nor `=>` nor `function` vs `const f =` is inside any
		// statement slice, so the verbatim text alone could not tell these apart
		// — the collapsed form round 10 replaced could. The shape is in the text
		// now.
		expect(GATE_BASELINE).toEqual([]);
		const concise = '\tconst handleGone = () => (onGone ?? onClose ?? navigateToCollectionRoot)();\n';
		expect(SOURCE.split(concise).length - 1).toBe(1);
		const braced = refusals(SOURCE.replace(concise, '\tconst handleGone = () => {\n\t\t(onGone ?? onClose ?? navigateToCollectionRoot)();\n\t};\n'));
		expect(braced.some((l) => l.includes('helper handleGone() has changed since it was reviewed')), `got ${JSON.stringify(braced)}`).toBe(true);
		const declared = '\tfunction identityHeld(captured: number): boolean {\n\t\treturn authStore.identityEpoch === captured;\n\t}\n';
		expect(SOURCE.split(declared).length - 1).toBe(1);
		const rebound = refusals(SOURCE.replace(declared, '\tconst identityHeld = (captured: number): boolean => {\n\t\treturn authStore.identityEpoch === captured;\n\t};\n'));
		expect(rebound.some((l) => l.includes('helper identityHeld() has changed since it was reviewed')), `got ${JSON.stringify(rebound)}`).toBe(true);
	});

	it('a helper reached only through a pulled DECLARATION is demanded and hashed (round 11 finding 2)', () => {
		// `const ops = { run: helper }` called as `ops.run()`: the unit never
		// names `helper`, so the helper pull — which used to read only the unit's
		// own roots — left its body outside every hash AND outside HELPERS, with
		// no refusal to say so. The declaration pull was transitive; this one was
		// not.
		const anchor = '\tfunction identityHeld(captured: number): boolean {\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		const call = '\t\t\t\tconst fresh = await sendTagBatch(saver);\n';
		expect(SOURCE.split(call).length - 1).toBe(1);
		const base = SOURCE.replace(
			anchor,
			'\tfunction sneakySave(): void {\n\t\tvoid 0;\n\t}\n\tconst sneakyOps = { run: sneakySave };\n\n' + anchor
		).replace(call, '\t\t\t\tsneakyOps.run();\n' + call);
		const opts = reviewAs(base);
		expect(opts.extraHelpers, 'the helper must be DEMANDED, not silently skipped').toHaveProperty('sneakySave');
		const edited = base.replace('\tfunction sneakySave(): void {\n\t\tvoid 0;\n\t}\n', '\tfunction sneakySave(): void {\n\t\titem = null;\n\t}\n');
		expect(refusals(edited, opts).some((l) => l.includes('helper sneakySave() has changed since it was reviewed'))).toBe(true);
	});

	it('a component-level binding form the gate does not model is REFUSED, not skipped (round 10 finding 3)', () => {
		// `import x = require('y')` binds a runtime name through a form
		// `topLevelOf` models neither as a declaration nor as a type. It refuses
		// rather than letting the name fall silently outside every hash. A
		// type-only form binds no value, so it stays accepted — the second leg.
		expect(GATE_BASELINE).toEqual([]);
		const anchor = '\tlet loadGeneration = 0;\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		const unmodelled = SOURCE.replace(anchor, "\timport legacyTags = require('$lib/items/openChildrenError');\n" + anchor);
		expect(
			refusals(unmodelled).some((l) => l.includes('TSImportEqualsDeclaration declaring legacyTags') && l.includes('is a binding form the gate does not model'))
		).toBe(true);
		const typeOnly = SOURCE.replace(anchor, '\tinterface LegacyTags {\n\t\tid: string;\n\t}\n' + anchor);
		expect(refusals(typeOnly), 'a type-only declaration binds no value and is not a gate concern').toEqual([]);
	});

	it('a component-level write target no binding pattern models is REFUSED, not skipped (round 9 F3)', () => {
		// `({ x: someMember.prop } = …)` binds through a MemberExpression inside a
		// pattern: the gate cannot say which names it rebinds, so it refuses
		// rather than reading it as "rebinds nothing". A BARE member write is a
		// different thing — not a rebinding at all — and stays accepted below.
		const anchor = '\tfunction identityHeld(captured: number): boolean {\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		const unmodelled = SOURCE.replace(anchor, '\tconst sink: { prop?: unknown } = {};\n\t({ x: sink.prop } = { x: 1 });\n\n' + anchor);
		expect(refusals(unmodelled).some((l) => l.includes('write target ObjectPattern') && l.includes('is not a modelled binding pattern'))).toBe(true);
		const bareMember = SOURCE.replace(anchor, '\tconst sink: { prop?: unknown } = {};\n\tsink.prop = 1;\n\n' + anchor);
		expect(refusals(bareMember), 'a bare member write is not a rebinding').toEqual([]);
	});

	it('CONTROL: an edit outside every hashed function is accepted', () => {
		const anchor = '<script lang="ts">\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		expect(refusals(SOURCE.replace(anchor, anchor + '\t// a note outside every fenced unit\n'))).toEqual([]);
	});

	it('CONTROL: an edit to a fenced unit costs exactly one hash bump', () => {
		const code = SOURCE.replace(
			TITLE_FENCE_AND_COMMITS,
			"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved(saveTok);\n\t\t\titem = withInflightTags(updated);\n"
		);
		const first = refusals(code);
		expect(first).toHaveLength(1);
		expect(first[0]).toContain('saveTitle() has changed since its row was reviewed');
		expect(refusals(code, { alsoReviewed: changedHashes(first) })).toEqual([]);
	});

	/** Reviews `base` as a fixture: its changed hashes and new helpers become accepted. */
	const reviewAs = (base: string): GateOptions => {
		const lines = refusals(base);
		const extraHelpers = Object.fromEntries(
			lines.flatMap((l) => {
				const m = l.match(/^helper (\w+)\(\) is reachable .*\(code ([0-9a-f]{12})\)$/);
				return m ? [[m[1]!, m[2]!]] : [];
			})
		);
		const opts = { alsoReviewed: changedHashes(lines), extraHelpers };
		expect(refusals(base, opts), 'the base does not pass once reviewed').toEqual([]);
		return opts;
	};

	it('a component-level declaration a unit reads is part of that unit\'s hash (refinement 2)', () => {
		const decl = '\tlet loadGeneration = 0;\n';
		expect(SOURCE.split(decl).length - 1).toBe(1);
		const got = refusals(SOURCE.replace(decl, '\tlet loadGeneration = $state(0);\n'));
		expect(got.some((l) => l.includes('saveTitle() has changed since its row was reviewed'))).toBe(true);
	});

	it('a helper reached only through another helper is hashed too (refinement 1)', () => {
		// switchedAway encloses no unit, so only a second hop reaches noteSaved.
		const anchor = '\tfunction switchedAway(targetItem: Item, gen: number): boolean {\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		const base = SOURCE.replace(anchor, '\tfunction noteSaved() {\n\t\tvoid 0;\n\t}\n\n' + anchor + '\t\tnoteSaved();\n');
		const opts = reviewAs(base);
		expect(opts.extraHelpers).toHaveProperty('noteSaved');
		const edited = base.replace('\tfunction noteSaved() {\n\t\tvoid 0;\n', "\tfunction noteSaved() {\n\t\titem = null;\n");
		expect(refusals(edited, opts).some((l) => l.includes('helper noteSaved() has changed since it was reviewed'))).toBe(true);
	});

	it('a function value rebound outside every unit changes the hash of the unit that only NAMES it (round 9 F4)', () => {
		// Step 1: a reviewed base where saveTitle hands `applyTitle` to someone
		// else rather than calling it — the round 8 G shape with the call removed.
		const noop = '\tlet applyTitle = (next: Item): void => {\n\t\tvoid next;\n\t};\n\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\tvoid next;\n\t\t};\n\t});\n\n\tfunction showSaved(saveTok: SaveToken) {\n';
		const base = SOURCE.replace('\tfunction showSaved(saveTok: SaveToken) {\n', noop).replace(
			TITLE_FENCE_AND_COMMITS,
			"{ title: titleDraft.trim() });\n\t\t\t[updated].forEach(applyTitle);\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"
		);
		const opts = reviewAs(base);
		// Step 2: only the rebinding inside the $effect changes. saveTitle never
		// calls applyTitle, so only the "names a function value" rule reaches it.
		const rebind = '\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\tvoid next;\n';
		expect(base.split(rebind).length - 1).toBe(1);
		expect(base).not.toContain('applyTitle(');
		const edited = base.replace(rebind, '\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\titem = withInflightTags(next);\n');
		expect(refusals(edited, opts).some((l) => l.includes('saveTitle() has changed since its row was reviewed'))).toBe(true);
	});

	it('a function value rebound outside every unit changes the hash of the unit that calls it (refinement 2)', () => {
		// Step 1: a reviewed base where saveTitle calls a no-op `let` helper.
		const noop = '\tlet applyTitle = (next: Item): void => {\n\t\tvoid next;\n\t};\n\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\tvoid next;\n\t\t};\n\t});\n\n\tfunction showSaved(saveTok: SaveToken) {\n';
		const base = SOURCE.replace('\tfunction showSaved(saveTok: SaveToken) {\n', noop).replace(
			TITLE_FENCE_AND_COMMITS,
			"{ title: titleDraft.trim() });\n\t\t\tapplyTitle(updated);\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved(saveTok);\n"
		);
		const opts = reviewAs(base);
		// Step 2: only the rebinding inside the $effect changes.
		const rebind = '\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\tvoid next;\n';
		expect(base.split(rebind).length - 1).toBe(1);
		const edited = base.replace(rebind, '\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\titem = withInflightTags(next);\n');
		expect(refusals(edited, opts).some((l) => l.includes('saveTitle() has changed since its row was reviewed'))).toBe(true);
	});
});
