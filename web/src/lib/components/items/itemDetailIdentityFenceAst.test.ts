/**
 * BUG-3084, ItemDetail — the POPULATION and COMMIT rules, on an AST (lead
 * ruling on checkpoint 51, after round 4 on #1387). `itemDetailIdentityFence
 * .test.ts` beside this keeps the site pins (listener, re-stamp, child props,
 * provider, reactive epoch reads); `itemDetailIdentityLoad.svelte.test.ts`
 * owns the semantics on a mount. The analysis itself, and what it cannot do,
 * is described in `src/test/identityFenceAst.ts`.
 *
 * Every unit the AST yields must match exactly one row below, and every row
 * exactly one unit. A row may list `may`: the commits that unit is allowed to
 * make while no fence holds, each covered by the row's reason. A key is an
 * assignment target's root name or a call's callee text.
 *
 * The round-4 mutants are the acceptance test: each is applied to an in-memory
 * copy of the component, and this guard must refuse it.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import {
	parseComponent,
	declarations,
	enumerateUnits,
	analyseUnit,
	walk,
	type AstSource,
	type Node,
	type Unit,
} from '../../../test/identityFenceAst';
import round4 from './itemDetailIdentityFence.round4.json';

interface GuardMutant {
	id: string;
	finding: string;
	what: string;
	old: string;
	new: string;
}
const ROUND4_MUTANTS: GuardMutant[] = round4.mutants;

const SOURCE = readFileSync(new URL('./ItemDetail.svelte', import.meta.url), 'utf8');

interface Row {
	why: string;
	may?: string[];
	/** Exact statement texts read as a bare await (their argument is not walked). */
	bareAwaits?: string[];
	/** Start the unit safe. Only with a `pin` proving why. */
	startSafe?: boolean;
	pin?: (src: AstSource, unit: Unit) => string | null;
	/**
	 * Callbacks this unit creates that may commit something, by callback key
	 * (see `AnalyseOptions.callbacks`). Every other callback it creates starts
	 * unsafe and may commit nothing.
	 */
	callbacks?: Record<string, { may: string[]; why: string }>;
}

/** Top-level `async function` declarations, by name. */
const ASYNC_FUNCTIONS: Record<string, Row> = {
	adoptOrConvergeToLiveCollection: { why: 'myGen against loadGeneration before adopting' },
	reconcileCollectionSegment: { why: 'identityHeld after the list fetch; retags and navigates only under it' },
	jumpToSection: { why: 'switches this instance\'s tab and scrolls to an anchor', may: ['document.getElementById', 'document.getElementById(anchorId).scrollIntoView'] },
	ensureGraphComp: { why: 'lazy-loads a component module into this instance', may: ['ItemGraphComp', 'graphLoadError'] },
	handleCopyRef: { why: 'switchedAway before the copied flag' },
	loadData: { why: 'IS the load: myGen against loadGeneration after every await' },
	startEditTitle: { why: 'focuses and sizes the input it opened synchronously', may: ['el', 'titleInputEl.focus', 'titleInputEl.setSelectionRange'] },
	saveTitle: { why: 'gen against loadGeneration on both arms' },
	updateField: {
		why: 'stillCurrent() on every arm, the OCC refetch and the open-children confirm',
		callbacks: {
			'submitOrderedOCC({send})': {
				may: ['api.items.update'],
				why: 'submitOrderedOCC calls send first with no await before it, and re-sends only after stillCurrent() with no await between (fieldWriteOrder.test.ts: "does not RE-SEND once the view moves on DURING the refetch")',
			},
			'submitOrderedOCC({refetch})': {
				may: ['api.items.get'],
				why: 'submitOrderedOCC refetches only after stillCurrent() with no await between (fieldWriteOrder.test.ts: "does not retry once the view has moved on"); a read',
			},
		},
	},
	flushTagSaver: {
		why: 'identityHeld(saver.epoch) before every commit and send; the unfenced writes are to this burst\'s own identity-stamped record, and the finally deletes that record only if the registry still holds it (the get)',
		may: ['saver', 'tagSavers.get', 'tagSavers.delete'],
	},
	refreshCollectionIfMoved: { why: 'gen against loadGeneration after the fetch' },
	loadTagSuggestions: { why: 'identityHeld after the fetch; the identity listener re-runs it' },
	stampSourceUrl: { why: 'switchedAway on both arms' },
	refreshFromSource: { why: 'switchedAway on every arm' },
	updateAssignedUser: { why: 'gen against loadGeneration on both arms' },
	updateAgentRole: { why: 'gen against loadGeneration on both arms' },
	flushRawIfPending: {
		why: 'genAtFlush against loadGeneration after each PATCH; the re-entrancy waiter returns state; the finally clears this drain\'s own in-flight flag',
		may: ['rawFlushInFlight'],
		bareAwaits: ['await new Promise((r) => setTimeout(r, 50));'],
	},
	refreshLinksPreservingOnFailure: { why: 'returns a value; its callers fence' },
	flushCollabBeforeRestore: { why: 'identityHeld before its failure toast' },
	closeCopyDialog: { why: 'restores focus after closing synchronously', may: ['paneMenuTrigger.focus'] },
	closePushDialog: { why: 'restores focus after closing synchronously', may: ['paneMenuTrigger.focus'] },
	flushContentBeforeCopy: { why: 'returns a boolean to the dialog' },
	handleCopied: { why: 'switchedAway before adopting the refreshed item' },
	handleDelete: { why: 'switchedAway on both arms' },
	handleRestore: { why: 'switchedAway on every arm' },
	handleDeleteLink: { why: 'switchedAway after each await' },
	handleCreateLink: { why: 'switchedAway after each await' },
	handleMove: { why: 'stillOnSource() on every arm, including inside navIfStillCurrent' },
};

interface SignedRow extends Row {
	/** Tested against the unit's function text (whitespace collapsed). */
	body: RegExp;
	/** For a continuation, tested against the deferring call's text up to the callback. */
	call?: RegExp;
}

/** Async functions that are not top-level declarations, in the script. */
const NESTED: SignedRow[] = [
	{ body: /event\.type === 'collection_updated'/, why: 'SSE: callbackGen after the collection fetch, itemGen on item branches' },
	{ body: /result\.type === 'caught_up'/, why: 'sync: callbackGen after the reconciliation, itemGen on item branches' },
	{ body: /flushCollabContent\(/, why: 'collab save: isForegroundCurrent (genAtFlush) before UI feedback' },
];

/** Async functions in the markup. */
const MARKUP: SignedRow[] = [
	{ body: /startGen/, why: 'Rich toggle: startGen against loadGeneration after each await' },
	{ body: /genAtToggle/, why: 'Markdown toggle: genAtToggle against loadGeneration after each await' },
];

const collapse = (s: string) => s.replace(/\s+/g, ' ');

function clearsBeforeFirstAwait(src: AstSource, fnName: string, timer: string): string | null {
	const fn = src.script.body.find((s: Node) => s.type === 'FunctionDeclaration' && s.id?.name === fnName);
	if (!fn) return `${fnName} is gone`;
	let firstAwait = Infinity;
	let clear = Infinity;
	walk(fn, (n) => {
		if (n.type === 'AwaitExpression') firstAwait = Math.min(firstAwait, n.start);
		if (n.type === 'CallExpression' && src.text(n).replace(/\s+/g, '') === `clearTimeout(${timer})`) clear = Math.min(clear, n.start);
	});
	return clear < firstAwait ? null : `${fnName} no longer clears ${timer} before its first await`;
}

/**
 * Why a continuation's timer is `timer`: its deferring call must be the whole
 * right-hand side of `timer = …` (round 5 P1-4 — a pin on the clear alone held
 * after the callback was moved onto a timer nobody clears).
 */
function assignedTo(src: AstSource, unit: Unit, timer: string): string | null {
	let ok = false;
	walk(src.script, (n) => {
		if (n.type === 'AssignmentExpression' && n.operator === '=' && n.right === unit.call && n.left.type === 'Identifier' && n.left.name === timer) ok = true;
	});
	return ok ? null : `its ${src.text(unit.call!.callee)} is not assigned to ${timer}`;
}

/** Callbacks passed to deferring calls, in the script. */
const CONTINUATIONS: SignedRow[] = [
	{ call: /noScroll: true, \}\)\.catch\($/, body: /./, why: 'rename heal failure: clears only the bridge object this heal installed', may: ['renameOverride'] },
	{ call: /^setTimeout\($/, body: /copied = false/, why: 'copy-flag reset: switchedAway' },
	{ call: /api\.items\.get\(wsSlug, itemSlug\)\.catch\($/, body: /./, why: 'loadData item fetch: sets a flag local to that load and re-throws' },
	{ call: /^setTimeout\($/, body: /staleConnecting = true/, why: 'connection state of this instance\'s own provider', may: ['staleConnecting'] },
	{ call: /\.get\(refreshCtx\.wsSlug, refreshCtx\.itemId\) \.then\($/, body: /./, why: 'force-refresh fetch: refreshGen against loadGeneration' },
	{ call: /forceRefreshNonce \+= 1; \}\) \.catch\($/, body: /./, why: 'force-refresh failure: refreshGen against loadGeneration' },
	{ call: /^setTimeout\($/, body: /^\(\) => \{ teardownFlushed = false; \}$/, why: 're-arms the BUG-3005 teardown latch, itself identity-checked', may: ['teardownFlushed'] },
	{ call: /^queueMicrotask\($/, body: /./, why: 'collab lazy seed: refuses a retired or re-identified context first' },
	{ call: /^setTimeout\($/, body: /^\(\) => \{ saveStatus = 'idle'; \}$/, why: 'cosmetic save-indicator reset', may: ['saveStatus'] },
	{ call: /^tick\(\)\.then\($/, body: /./, why: 'schedules a focus frame; commits nothing itself' },
	{ call: /^requestAnimationFrame\($/, body: /./, why: 'focuses the editor after a tab switch', may: ['editorInstance.commands.focus'] },
	{
		call: /^setTimeout\($/,
		body: /content: toSave \}\)\.then/,
		why: 'content debounce: loadData clears this timer before its first await, so the callback never runs across a load',
		startSafe: true,
		pin: (src, unit) => clearsBeforeFirstAwait(src, 'loadData', 'contentDebounceTimer') ?? assignedTo(src, unit, 'contentDebounceTimer'),
	},
	{ call: /\{ content: toSave \}\)\.then\($/, body: /^\(\) =>/, why: 'content save: switchedAway' },
	{ call: /showSaved\(\); \}\)\.catch\($/, body: /./, why: 'content save failure: switchedAway' },
	{ call: /\{ keepalive: true \}\) \.then\($/, body: /./, why: 'raw keepalive save: genAtSave against loadGeneration' },
	{ call: /localDirty = false; \} \}\) \.catch\($/, body: /^\(\) => \{\}$/, why: 'raw keepalive failure: empty' },
	{ call: /reqItemId, \{ content: toSave \}\)\.then\($/, body: /./, why: 'raw foreground save: genAtSave against loadGeneration' },
	{ call: /content: item\.content \}\); \} \}\)\.catch\($/, body: /./, why: 'raw foreground failure: genAtSave against loadGeneration' },
];

/** Deferring calls whose callback is not a function literal. */
const IDENTIFIER_CALLBACKS: Array<{ text: string; count: number; why: string }> = [
	{ text: 'Promise.resolve().then(ensureGraphComp)', count: 2, why: 'ensureGraphComp is itself a unit in the table above' },
	{ text: 'setTimeout(r, 50)', count: 1, why: 'resolves flushRawIfPending\'s re-entrancy waiter' },
];

function unitLabel(src: AstSource, u: Unit): string {
	return u.name ? `${u.name}()` : `${u.kind}${u.inMarkup ? ' in markup' : ''} at line ${u.line}`;
}

/** Everything this guard refuses about `code`, as readable lines. Empty means clean. */
export function refusals(code: string): string[] {
	const out: string[] = [];
	let src: AstSource;
	try {
		src = parseComponent(code);
	} catch (e) {
		return [`does not parse: ${String(e)}`];
	}
	const decls = declarations(src);
	const { units, deferringCalls } = enumerateUnits(src);

	const claim = (rows: SignedRow[], members: Unit[], what: string, callText: (u: Unit) => string) => {
		const hits = new Map<SignedRow, Unit[]>();
		for (const u of members) {
			const body = collapse(src.text(u.fn));
			const matched = rows.filter((r) => r.body.test(body) && (!r.call || r.call.test(callText(u))));
			if (matched.length !== 1) {
				out.push(`${what} ${unitLabel(src, u)} matches ${matched.length} table rows — disposition it: ${body.slice(0, 80)}`);
				continue;
			}
			hits.set(matched[0]!, [...(hits.get(matched[0]!) ?? []), u]);
		}
		for (const r of rows) {
			const n = hits.get(r)?.length ?? 0;
			if (n !== 1) out.push(`${what} row (${r.why}) matches ${n} units — it must match exactly one`);
		}
		return hits;
	};

	const rowFor = new Map<Unit, Row>();
	const topLevel = units.filter((u) => u.kind === 'async-function' && u.name);
	const names = topLevel.map((u) => u.name!).sort();
	const expected = Object.keys(ASYNC_FUNCTIONS).sort();
	for (const n of names) if (!(n in ASYNC_FUNCTIONS)) out.push(`async function ${n}() is not in the table — disposition it`);
	for (const n of expected) if (!names.includes(n)) out.push(`the table names ${n}(), which is gone`);
	for (const u of topLevel) if (ASYNC_FUNCTIONS[u.name!]) rowFor.set(u, ASYNC_FUNCTIONS[u.name!]!);

	const callText = (u: Unit) => collapse(code.slice(u.call!.start, u.fn.start));
	for (const [row, us] of claim(NESTED, units.filter((u) => u.kind === 'async-function' && !u.name && !u.inMarkup), 'nested async function', callText)) {
		for (const u of us) rowFor.set(u, row);
	}
	for (const [row, us] of claim(MARKUP, units.filter((u) => u.kind === 'async-function' && u.inMarkup), 'markup async function', callText)) {
		for (const u of us) rowFor.set(u, row);
	}
	const markupContinuations = units.filter((u) => u.kind === 'continuation' && u.inMarkup);
	for (const u of markupContinuations) out.push(`the markup defers a continuation at line ${u.line} — move it into a script handler the table covers`);
	for (const [row, us] of claim(CONTINUATIONS, units.filter((u) => u.kind === 'continuation' && !u.inMarkup), 'continuation', callText)) {
		for (const u of us) rowFor.set(u, row);
	}

	const idCalls = deferringCalls.filter((c) => !c.arguments.some((a: Node) => a.type.endsWith('FunctionExpression')));
	for (const row of IDENTIFIER_CALLBACKS) {
		const n = idCalls.filter((c) => src.text(c).replace(/\s+/g, '') === row.text.replace(/\s+/g, '')).length;
		if (n !== row.count) out.push(`${row.text} occurs ${n} times, the table says ${row.count}`);
	}
	for (const c of idCalls) {
		const t = src.text(c).replace(/\s+/g, '');
		if (!IDENTIFIER_CALLBACKS.some((r) => r.text.replace(/\s+/g, '') === t)) out.push(`deferred call with a non-literal callback at line ${src.line(c.start)} is not tabled: ${t.slice(0, 80)}`);
	}

	for (const u of units) {
		const row = rowFor.get(u);
		if (!row) continue;
		if (row.startSafe) {
			const why = row.pin ? row.pin(src, u) : 'a row that starts safe has no pin';
			if (why) out.push(`${unitLabel(src, u)} starts safe, but ${why}`);
		}
		try {
			const v = analyseUnit(src, decls, u, {
				startSafe: u.kind === 'async-function' || !!row.startSafe,
				may: new Set(row.may ?? []),
				callbacks: new Map(Object.entries(row.callbacks ?? {}).map(([k, v]) => [k, new Set(v.may)])),
				bareAwaits: new Set(row.bareAwaits ?? []),
			});
			for (const x of v) out.push(`${unitLabel(src, u)} line ${x.line}: ${x.what} — ${x.text}`);
		} catch (e) {
			out.push(`${unitLabel(src, u)}: ${String((e as Error).message ?? e)}`);
		}
	}
	return out;
}

describe('ItemDetail: every async unit is tabled, and none commits past an unfenced await (AST)', () => {
	it('the component as written is clean', () => {
		expect(refusals(SOURCE)).toEqual([]);
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

/** saveTitle's fence and the two commits under it: the anchor most round-5 edits rewrite. */
const TITLE_FENCE_AND_COMMITS =
	"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n";

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
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen === loadGeneration && (await dialogs.confirm('Keep the new title?'))) {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t\tshowSaved();\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E1b a fence boolean whose initialiser awaits after the fence',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst keep = gen === loadGeneration && (await dialogs.confirm('Keep the new title?'));\n\t\t\tif (!keep) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E1c the early-return form: fence, then an await, in one || test',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id || !(await dialogs.confirm('Keep the new title?'))) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
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
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\trequestIdleCallback(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t\tshowSaved();\n\t\t\t});\n"]],
			refuses: ['saveTitle()', 'callback requestIdleCallback(…)', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E8b a commit inside an event listener',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\twindow.addEventListener('focus', () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t}, { once: true });\n"]],
			refuses: ['saveTitle()', 'callback window.addEventListener(…)', 'assigns item after an unfenced await'],
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
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tfor (const newer of await api.items.newerVersions(wsSlug, targetItem.id)) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P2-2 a commit after a while whose test awaits and whose body always returns',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\twhile (await api.items.hasNewer(wsSlug, targetItem.id)) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
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
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tswitch (titleDraft) {\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\treturn;\n\t\t\t}\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
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
			// An inlined helper resolves names in ITS scopes: a caller's local that
			// shares a name with the state the helper writes excuses nothing.
			id: "R5 P1-3 a caller's local does not excuse the same name inside an inlined helper",
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst saveStatus = updated.title;\n\t\t\tshowSaved();\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n"]],
			refuses: ['saveTitle()', 'showSaved() -> assigns saveStatus after an unfenced await'],
		},
	];
	it('a callback allowance that names no callback the unit creates is refused', () => {
		const src = parseComponent(SOURCE);
		const unit = enumerateUnits(src).units.find((u) => u.name === 'updateField')!;
		expect(() =>
			analyseUnit(src, declarations(src), unit, { startSafe: true, callbacks: new Map([['submitOrderedOCC({sendd})', new Set(['api.items.update'])]]) })
		).toThrow(/names no callback this unit creates/);
	});

	it.each(ANALYSIS_DEFECTS.map((d) => [d.id, d] as const))('analysis defect stays closed: %s', (_id, d) => {
		expect(BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of d.subs) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		const got = refusals(code);
		expect(
			got.some((line) => d.refuses.every((s) => line.includes(s))),
			`no refusal carries ${JSON.stringify(d.refuses)}; got ${JSON.stringify(got, null, 1)}`
		).toBe(true);
	});

	/**
	 * Controls: a guard that refuses everything also refuses every mutant. These
	 * edits commit nothing and must stay accepted.
	 */
	const ACCEPTED: Array<{ id: string; old: string; new: string }> = [
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
			id: 'a commit moved BELOW its fence stays accepted',
			old: "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n",
			new: "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved();\n\t\t\titem = withInflightTags(updated);\n",
		},
	];
	it.each(ACCEPTED.map((c) => [c.id, c] as const))('control accepted: %s', (_id, c) => {
		expect(BASELINE).toEqual([]);
		expect(SOURCE.split(c.old).length - 1).toBe(1);
		expect(refusals(SOURCE.replace(c.old, c.new))).toEqual([]);
	});
});
