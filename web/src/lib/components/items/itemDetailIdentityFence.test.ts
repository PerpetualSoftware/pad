/**
 * BUG-3084, ItemDetail — the SOURCE half. `itemDetailIdentityLoad.svelte.test.ts`
 * beside this file owns the SEMANTICS on a mount; this owns the POPULATION.
 *
 * THE DESIGN THIS GUARD HOLDS (BUG-3084 checkpoints 32-33, lead ruling): on this
 * surface an identity change IS A LOAD. ItemDetail's async commit points were
 * already switch-safe through three generation counters — `loadGeneration`,
 * `itemGen`, `collectionGen` — and `loadData` bumps all three. So an identity
 * listener that calls `loadData` refuses every generation-fenced continuation
 * by construction, and the per-handler capture the other surfaces use would
 * duplicate a fence this file already has. What is left for an explicit
 * `identityHeld` is the short list of sites whose staleness checks are keyed on
 * something an identity change does not move.
 *
 * So the rule, per enumerated block, is: its commit points are gated by a
 * generation `loadData` bumps, OR by `identityHeld`, OR the block commits
 * nothing an identity owns — and the table below says which, with the reason.
 * A block the table does not name fails the build.
 *
 * WHAT THIS CANNOT DO, as for every guard in the family: it checks SPELLINGS.
 * "A generation token appears after the first await" does not prove the check
 * guards the commit it sits beside. The mount suite drives one leg per fence
 * kind and a control per refusal; the header of `identityFenceSource.ts` says
 * why neither instrument is enough alone.
 *
 * THE PAGE-LOAD EPOCH IS ALLOWED HERE, which it is not on the route surfaces.
 * `identityEpochAtLoad` answers "whose typing is in the editor", and the
 * BUG-3005 teardown writes compare against it correctly. It is held to its six
 * known sites, and no handler in the tables below may read it.
 */
import { describe, it, expect } from 'vitest';
import {
	readFenceSource,
	trackedEpochReadDetails,
	withoutCatchArms,
	matchDelimiter,
	type EnumeratedBlock,
} from '../../../test/identityFenceSource';

const src = readFenceSource(new URL('./ItemDetail.svelte', import.meta.url));
const SCRIPT = src.script;

type Disposition =
	| { kind: 'generation'; why: string }
	| { kind: 'identity'; why: string; capturedBy?: { caller: string; field: string } }
	| { kind: 'none'; why: string };

/**
 * Tokens that name a generation `loadData` bumps, or a helper built on one.
 *
 * NOT `adoptCollection(`: `shouldAdoptCollection` deliberately accepts a STALE
 * generation when it corrects the shown collection to the live item's, so it
 * is not a fence against an identity change (codex round 1 on #1387).
 */
const GENERATION = /switchedAway\(|[!=]==\s*loadGeneration|loadGeneration\s*[!=]==|[!=]==\s*itemGen|itemGen\s*[!=]==|stillCurrent\(\)|stillOnSource\(\)|isForegroundCurrent\(\)/g;
/** Either kind of fence, for the per-await count. */
const ANY_FENCE = new RegExp(`${GENERATION.source}|identityHeld\\(`, 'g');

const ASYNC_FUNCTIONS: Record<string, Disposition> = {
	adoptOrConvergeToLiveCollection: { kind: 'generation', why: 'myGen against loadGeneration before adoptCollection' },
	reconcileCollectionSegment: { kind: 'identity', why: 'keyed on route + collection id, which a same-item reload leaves equal; retags and navigates' },
	jumpToSection: { kind: 'none', why: 'switches this instance\'s tab and scrolls; no data, nothing an identity owns' },
	ensureGraphComp: { kind: 'none', why: 'lazy-imports a component module' },
	handleCopyRef: { kind: 'generation', why: 'switchedAway before the copied flag' },
	loadData: { kind: 'generation', why: 'IS the load: myGen against loadGeneration at every await' },
	startEditTitle: { kind: 'none', why: 'awaits a tick to focus an input it opened synchronously' },
	saveTitle: { kind: 'generation', why: 'gen !== loadGeneration on both arms' },
	updateField: { kind: 'generation', why: 'stillCurrent() (loadGeneration) on every arm, the OCC refetch and the open-children confirm' },
	flushTagSaver: {
		kind: 'identity',
		why: 'drain keyed on item id; a same-item reload passes it and the queued batch would go out on the new cookie',
		// The burst is typed in `updateTags`, so that is where its identity is
		// captured; the drain compares against the burst's record of it.
		capturedBy: { caller: 'updateTags', field: 'saver.epoch' },
	},
	refreshCollectionIfMoved: { kind: 'generation', why: 'gen against loadGeneration beside adoptCollection, which accepts a stale generation by design' },
	loadTagSuggestions: { kind: 'identity', why: 'keyed on the workspace slug alone; the listener re-runs it' },
	stampSourceUrl: { kind: 'generation', why: 'switchedAway on both arms' },
	refreshFromSource: { kind: 'generation', why: 'switchedAway on every arm' },
	updateAssignedUser: { kind: 'generation', why: 'gen !== loadGeneration on both arms' },
	updateAgentRole: { kind: 'generation', why: 'gen !== loadGeneration on both arms' },
	flushRawIfPending: { kind: 'generation', why: 'genAtFlush against loadGeneration after each PATCH; its other await is the re-entrancy waiter, which returns state and commits nothing' },
	refreshLinksPreservingOnFailure: { kind: 'none', why: 'returns a value; both callers gate their commit on itemGen' },
	flushCollabBeforeRestore: { kind: 'identity', why: 'its only post-await commit is a toast, and nothing it checks moves on an identity change' },
	closeCopyDialog: { kind: 'none', why: 'awaits a tick to restore focus after closing synchronously' },
	closePushDialog: { kind: 'none', why: 'awaits a tick to restore focus after closing synchronously' },
	flushContentBeforeCopy: { kind: 'none', why: 'returns a boolean to the dialog; the load an identity change runs closes that dialog' },
	handleCopied: { kind: 'generation', why: 'switchedAway before adopting the refreshed item; the toast precedes the await' },
	handleDelete: { kind: 'generation', why: 'switchedAway on both arms' },
	handleRestore: { kind: 'generation', why: 'switchedAway on every arm' },
	handleDeleteLink: { kind: 'generation', why: 'switchedAway after each await' },
	handleCreateLink: { kind: 'generation', why: 'switchedAway after each await' },
	handleMove: { kind: 'generation', why: 'stillOnSource() (switchedAway) on every arm' },
};

/**
 * Blocks the other enumerators return, matched by a SIGNATURE in the body
 * rather than by position, so a reordering cannot silently re-point a
 * disposition at a different block. Every block must match exactly one row and
 * every row exactly one block.
 */
interface SignedDisposition {
	signature: RegExp;
	disposition: Disposition;
}

const NESTED_CALLBACKS: SignedDisposition[] = [
	{ signature: /event\.type === 'collection_updated'/, disposition: { kind: 'generation', why: 'SSE: callbackGen (captured at entry) after the collection fetch, item branches on itemGen captured after it' } },
	{ signature: /result\.type === 'caught_up'/, disposition: { kind: 'generation', why: 'sync: callbackGen after the awaited reconciliation, item branches on itemGen captured after it' } },
	{ signature: /flushCollabContent\(/, disposition: { kind: 'generation', why: 'collab save: UI feedback gated on genAtFlush against loadGeneration' } },
];

const TIMERS: SignedDisposition[] = [
	{ signature: /copied = false/, disposition: { kind: 'generation', why: 'switchedAway before clearing the copied flag' } },
	{ signature: /staleConnecting = true/, disposition: { kind: 'none', why: 'connection state of this instance\'s own provider' } },
	{ signature: /teardownFlushed = false/, disposition: { kind: 'none', why: 're-arms the BUG-3005 teardown latch, which is itself identity-checked' } },
	{ signature: /saveStatus = 'idle'; \}$/, disposition: { kind: 'none', why: 'cosmetic save-indicator reset; loadData clears this timer' } },
	{ signature: /content: toSave/, disposition: { kind: 'generation', why: 'content debounce: switchedAway on both arms; loadData cancels it' } },
	{ signature: /^setTimeout\(r, 50\)$/, disposition: { kind: 'none', why: 'no body: the wait inside flushRawIfPending\'s re-entrancy loop' } },
];

const MARKUP_ARROWS: Disposition = {
	kind: 'generation',
	why: 'the Rich/Markdown toggles: startGen / genAtToggle against loadGeneration after each await',
};

function firstAwait(body: string, label: string): number {
	const at = body.search(/\bawait\b/);
	expect(at, `${label} has no await — re-point its disposition`).toBeGreaterThan(-1);
	return at;
}

/**
 * STATEMENTS excised from a handler's body before the per-await rules, each
 * with the reason it commits nothing. A statement, not a function: skipping
 * the whole function left `flushRawIfPending`'s real PATCH await unchecked
 * (round 3 on #1387). Each must occur exactly once in its handler.
 */
const PER_AWAIT_EXCISED: Record<string, { statement: string; why: string }> = {
	'flushRawIfPending()': {
		statement: 'await new Promise((r) => setTimeout(r, 50));',
		why: 'the re-entrancy waiter returns pending state and commits nothing',
	},
};

/**
 * COUNTED, not merely present (codex round 1 on #1387): removing only
 * `saveTitle`'s SUCCESS-arm check passed a presence rule, because the catch
 * arm's token satisfied it. The success path (catch arms excised) must carry at
 * least one fence per await. A LOWER BOUND, not a proof of position — the
 * mount suite's legs are the other half.
 */
function holdsPerAwait(label: string, fullBody: string) {
	let body = fullBody;
	const excised = PER_AWAIT_EXCISED[label];
	if (excised) {
		expect(body.split(excised.statement).length - 1, `${label}: the excised statement (${excised.why}) is not there exactly once — re-point this table`).toBe(1);
		body = body.replace(excised.statement, ' '.repeat(excised.statement.length));
	}
	const success = withoutCatchArms(body);
	const awaits = (success.match(/\bawait\b/g) ?? []).length;
	if (awaits === 0) return;
	const after = success.slice(success.search(/\bawait\b/));
	const fences = (after.match(ANY_FENCE) ?? []).length;
	expect(fences, `${label}: ${awaits} awaits on its success path but ${fences} fence checks after the first — a continuation commits unguarded`).toBeGreaterThanOrEqual(awaits);
	// POSITIONAL, not only counted (the restore follow-up GET, codex round 1 on
	// #1387): a `finally` arm's check satisfies the count while the continuation
	// between two awaits issues its next request unchecked. Between every pair
	// of consecutive awaits on the success path there must be a fence.
	// NOTHING COMMITS BEFORE THE FENCE (codex round 2 on #1387): moving
	// `item = …` above its existing check kept every count and every position
	// above satisfied while the stale response landed. After each await, the text
	// up to the first fence may not assign component state or call a committer.
	const COMMIT = new RegExp(
		[
			// A statement-leading assignment or increment.
			String.raw`(?:^|[;{}\n])\s*(?!const\b|let\b|var\b|return\b|if\b|else\b)[A-Za-z_$][\w$]*(?:\.[\w$]+)*\s*(?:=(?!=)|\+\+|--)`,
			// The same behind a brace-less `if (…)` or `else` (round 3 on #1387).
			String.raw`\b(?:if\s*\([^()]*(?:\([^()]*\)[^()]*)*\)|else)\s*[A-Za-z_$][\w$]*(?:\.[\w$]+)*\s*(?:=(?!=)|\+\+|--)`,
			// A parenthesised assignment: `fresh && (item = …)`.
			String.raw`\(\s*[A-Za-z_$][\w$]*(?:\.[\w$]+)*\s*=(?![=>])`,
			// Committers: UI, navigation, stores, and the mutating services a
			// continuation can drive without assigning anything (round 3 on #1387).
			String.raw`\b(?:toastStore\.show|showSaved|handleGone|goto|adoptCollection|collectionStore\.\w+|editorStore\.\w+|handleNavigateAway|tagSavers\.(?:set|delete|clear)|localIndex\.\w+|syncService\.\w+|rawContentSaver\.\w+|collabFlusher\.\w+)\(`,
		].join('|')
	);
	success.split(/\bawait\b/).slice(1).forEach((seg, i) => {
		// The awaited EXPRESSION ends where its call chain does, not at the first
		// newline: `await Promise.all([ a.catch((e) => { flag = true; }), … ])`
		// spans lines, and its argument bodies are not continuations of the await.
		let end = 0;
		for (;;) {
			const open = seg.slice(end).search(/[([]/);
			if (open === -1) break;
			const at = end + open;
			if (end > 0 && /[;\n]/.test(seg.slice(end, at).trim())) break;
			if (end === 0 && /[;]/.test(seg.slice(0, at))) break;
			const close = matchDelimiter(seg, at, seg[at]!, seg[at] === '(' ? ')' : ']');
			if (close === -1) throw new Error(`${label}: could not delimit the expression after await ${i + 1} — re-point this guard`);
			end = close + 1;
			if (!/^\s*\.\s*[A-Za-z_$]/.test(seg.slice(end))) break;
		}
		const stmtEnd = seg.slice(end).search(/[;\n]/);
		const rest = seg.slice(stmtEnd === -1 ? seg.length : end + stmtEnd + 1);
		const fenceAt = rest.search(new RegExp(ANY_FENCE.source));
		const before = fenceAt === -1 ? rest : rest.slice(0, fenceAt);
		if (fenceAt === -1 && i === (success.match(/\bawait\b/g) ?? []).length - 1) {
			// The last await's continuation must still meet a fence before a commit.
			expect(before, `${label}: commits after its last await with no fence at all`).not.toMatch(COMMIT);
			return;
		}
		expect(before, `${label}: commits after await ${i + 1} BEFORE its fence — a stale response lands first`).not.toMatch(COMMIT);
	});
	const segments = success.split(/\bawait\b/).slice(1, -1);
	segments.forEach((seg, i) => {
		expect(
			(seg.match(ANY_FENCE) ?? []).length,
			`${label}: no fence between await ${i + 1} and await ${i + 2} on its success path — the next request goes out unchecked`
		).toBeGreaterThan(0);
	});
}

function holdsDisposition(label: string, body: string, d: Disposition) {
	if (d.kind !== 'none') holdsPerAwait(label, body);
	const at = firstAwait(body, label);
	const after = body.slice(at);
	if (d.kind === 'generation') {
		expect(after.match(GENERATION)?.length ?? 0, `${label}: dispositioned GENERATION (${d.why}) but no generation check follows its first await`).toBeGreaterThan(0);
	} else if (d.kind === 'identity' && d.capturedBy) {
		const field = d.capturedBy.field.replace('.', '\\.');
		expect(after, `${label}: dispositioned IDENTITY via ${d.capturedBy.field} but never checks it after its first await`).toMatch(
			new RegExp(`identityHeld\\(${field}\\)`)
		);
		// The capture's own rule ("the tag burst records its identity") holds
		// the caller; this only asserts the caller is still the one named.
		expect(SCRIPT, `${d.capturedBy.caller} no longer exists — re-point this disposition`).toMatch(
			new RegExp(`function ${d.capturedBy.caller}\\(`)
		);
	} else if (d.kind === 'identity') {
		expect(body.slice(0, at), `${label}: dispositioned IDENTITY (${d.why}) but captures no identity before its first await`).toMatch(
			/captureIdentity\(\)/
		);
		expect(after, `${label}: dispositioned IDENTITY (${d.why}) but checks identityHeld nowhere after its first await`).toMatch(/identityHeld\(/);
	}
	// loadData is the one block that WRITES it (the re-stamp, held by its own
	// rule below); no other handler may read it.
	if (label !== 'loadData()') {
		expect(body, `${label} reads the page-load epoch, which belongs to the teardown writes only`).not.toMatch(/identityEpochAtLoad/);
	}
}

function matchSigned(blocks: EnumeratedBlock[], table: SignedDisposition[], what: string) {
	expect(blocks.length, `${what}: the page has ${blocks.length}, the table names ${table.length} — disposition the new one`).toBe(table.length);
	for (const block of blocks) {
		const rows = table.filter((r) => r.signature.test(block.body.trim()));
		expect(rows.length, `${what} ${block.label} matches ${rows.length} table rows — it must match exactly one`).toBe(1);
	}
	for (const row of table) {
		const hits = blocks.filter((b) => row.signature.test(b.body.trim()));
		expect(hits.length, `${what} row ${row.signature} matches ${hits.length} blocks — it must match exactly one`).toBe(1);
	}
}

describe('ItemDetail: the population is closed and every member is dispositioned', () => {
	it('every top-level async function is in the table, and the table names nothing that is gone', () => {
		const names = [...src.asyncFunctions().keys()].sort();
		expect(names).toEqual(Object.keys(ASYNC_FUNCTIONS).sort());
	});

	it('every async function holds its disposition', () => {
		for (const [name, body] of src.asyncFunctions()) {
			holdsDisposition(`${name}()`, body, ASYNC_FUNCTIONS[name]!);
		}
	});

	it('every nested async callback is dispositioned by signature and holds it', () => {
		const blocks = src.nestedAsyncCallbacks();
		matchSigned(blocks, NESTED_CALLBACKS, 'nested async callback');
		for (const block of blocks) {
			const row = NESTED_CALLBACKS.find((r) => r.signature.test(block.body.trim()))!;
			holdsDisposition(block.label, block.body, row.disposition);
		}
	});

	it('every deferred timer is dispositioned by signature', () => {
		const blocks = src.deferredTimers();
		matchSigned(blocks, TIMERS, 'timer');
		for (const block of blocks) {
			const row = TIMERS.find((r) => r.signature.test(block.body.trim()))!;
			if (row.disposition.kind === 'generation') {
				expect(block.body.match(GENERATION)?.length ?? 0, `${block.label}: dispositioned GENERATION (${row.disposition.why}) with no generation check`).toBeGreaterThan(0);
			}
		}
	});

	it('both markup async arrows are the mode toggles and hold the generation disposition', () => {
		const arrows = src.markupAsyncArrows();
		expect(arrows.length, 'a markup async arrow was added or removed — disposition it').toBe(2);
		const toggles = [...src.markup.matchAll(/onclick=\{async \(\) => \{/g)].map((m) => m.index!);
		expect(toggles.length).toBe(2);
		for (const at of toggles) {
			const body = src.markup.slice(at, src.markup.indexOf('title="', at));
			holdsDisposition('mode toggle', body, MARKUP_ARROWS);
		}
	});
});

describe('ItemDetail: captures are taken where they mean something (round 3 hardening on #1387)', () => {
	/**
	 * A capture of a generation or an identity AFTER an await is tautological
	 * unless something between that await and the capture proves the load is
	 * still current: the capture would record the NEW load, and every fence
	 * compared against it would pass. So a capture after an await needs a
	 * load-generation or identity fence since the nearest await before it. The
	 * SSE and sync callbacks' `myItemGen = itemGen` are the members that
	 * need this today: each follows its `callbackGen` check with no await
	 * between them.
	 */
	const CAPTURE = /(?<![!=<>])=\s*(?:\+\+\s*)?(?:loadGeneration|itemGen|collectionGen)\b(?!\s*[!=]==)|(?<![!=]==\s*|=>\s*)captureIdentity\(\)/g;
	const LOAD_FENCE = /[!=]==\s*loadGeneration|loadGeneration\s*[!=]==|switchedAway\(|stillCurrent\(\)|stillOnSource\(\)|identityHeld\(/;

	/**
	 * Blocks that end in an unconditional `return;` and close before `at`:
	 * control inside them never reaches `at`. The SSE and sync callbacks await
	 * inside such branches TEXTUALLY before their later captures, and on the
	 * captures' own paths no await precedes them (SSE) or the only one is
	 * followed by `callbackGen` (sync). The grammar is this file's: a branch
	 * that falls through ends in something other than `return;`.
	 */
	function deadBlocksBefore(body: string, at: number): Array<[number, number]> {
		const out: Array<[number, number]> = [];
		for (let i = body.indexOf('{'); i !== -1 && i < at; i = body.indexOf('{', i + 1)) {
			const close = matchDelimiter(body, i, '{', '}');
			if (close === -1 || close >= at) continue;
			if (/\breturn;\s*$/.test(body.slice(i + 1, close))) out.push([i, close]);
		}
		return out;
	}

	it('every capture after an await follows a load or identity fence since that await', () => {
		const blocks: Array<[string, string]> = [...src.asyncFunctions()].map(([n, b]) => [`${n}()`, b]);
		for (const b of src.nestedAsyncCallbacks()) blocks.push([b.label, b.body]);
		let checked = 0;
		let skippedDead = 0;
		for (const [label, full] of blocks) {
			const ex = PER_AWAIT_EXCISED[label];
			const text = ex ? full.replace(ex.statement, ' '.repeat(ex.statement.length)) : full;
			for (const m of text.matchAll(CAPTURE)) {
				// Mask every returning branch that closes before the capture: its
				// awaits are not on this path, and neither are its fences.
				let body = text;
				for (const [o, c] of deadBlocksBefore(text, m.index!)) {
					body = body.slice(0, o) + ' '.repeat(c - o + 1) + body.slice(c + 1);
				}
				const all = [...text.matchAll(/\bawait\b/g)].filter((x) => x.index! < m.index!).length;
				const prior = [...body.matchAll(/\bawait\b/g)].map((x) => x.index!).filter((x) => x < m.index!);
				if (prior.length < all) skippedDead++;
				if (prior.length === 0) continue;
				checked++;
				const since = body.slice(prior[prior.length - 1]!, m.index!);
				expect(since, `${label}: captures \`${m[0]}\` after an await with no load fence since it — every later check against it passes`).toMatch(LOAD_FENCE);
			}
		}
		// Non-vacuity, both halves: the sync callback's two item captures follow
		// its awaited reconciliation, and four captures sit textually after a
		// returning branch's await.
		expect(checked, 'no post-await capture was checked — the pattern no longer reads this file').toBeGreaterThan(0);
		expect(skippedDead, 'no capture sat after a returning branch — re-read the dead-branch rule').toBeGreaterThan(0);
	});
});

describe('ItemDetail: an identity change is a load', () => {
	function listenerBody(): string {
		const at = SCRIPT.indexOf('authStore.onIdentityChange(');
		expect(at, 'the identity listener is gone — without it no generation moves on an identity change').toBeGreaterThan(-1);
		expect(SCRIPT.indexOf('authStore.onIdentityChange(', at + 1), 'a second identity listener — decide which one loads').toBe(-1);
		const open = SCRIPT.indexOf('{', at);
		const end = SCRIPT.indexOf('\n\t});', open);
		expect(end, 'could not delimit the identity listener — re-point this guard').toBeGreaterThan(open);
		return SCRIPT.slice(open, end);
	}

	it('the listener calls loadData and re-runs the tag suggestions, and is released on destroy', () => {
		const body = listenerBody();
		expect(body).toMatch(/\bloadData\(\)/);
		expect(body).toMatch(/loadTagSuggestions\(wsSlug\)/);
		expect(SCRIPT).toMatch(/const (\w+) = authStore\.onIdentityChange\([\s\S]*?\n\t\}\);\n\tonDestroy\(\1\);/);
	});

	it('loadData bumps all three generations before its first await', () => {
		const body = src.asyncFunctions().get('loadData')!;
		const head = body.slice(0, firstAwait(body, 'loadData()'));
		expect(head).toMatch(/\+\+loadGeneration/);
		expect(head).toMatch(/\+\+collectionGen/);
		expect(head).toMatch(/\+\+itemGen/);
	});

	it('loadData re-stamps the load epoch AFTER the keepalive raw flush and the clear, before its first await, untracked', () => {
		const body = src.asyncFunctions().get('loadData')!;
		const restamp = body.search(/identityEpochAtLoad\s*=\s*untrack\(\(\)\s*=>\s*authStore\.identityEpoch\)/);
		expect(restamp, 'the re-stamp is gone, or reads the epoch TRACKED (the route effect would reload on every identity change)').toBeGreaterThan(-1);
		expect(body.match(/identityEpochAtLoad\s*=/g)?.length, 'loadData re-stamps more than once').toBe(1);
		const flush = body.indexOf('rawContentSaver.flushNow({ keepalive: true })');
		const clear = body.indexOf('rawContentSaver.clearPending()');
		expect(flush, 'the keepalive raw flush moved — re-point this guard').toBeGreaterThan(-1);
		expect(clear, 'the raw saver clear moved — re-point this guard').toBeGreaterThan(-1);
		expect(restamp, 'the re-stamp precedes the keepalive flush, so the flush\'s identity check compares two equal epochs').toBeGreaterThan(flush);
		expect(restamp).toBeGreaterThan(clear);
		expect(restamp).toBeLessThan(firstAwait(body, 'loadData()'));
	});

	it('the tag-suggestions effect untracks its call, so the entry capture is not a dependency', () => {
		const effects = src.effectBlocks().filter((b) => /loadTagSuggestions\(/.test(b.body));
		expect(effects.length).toBe(1);
		expect(effects[0]!.body).toMatch(/untrack\(\(\) => loadTagSuggestions\(ws\)\)/);
	});

	it('the listener clears the workspace-keyed member and role caches before its load', () => {
		const at = SCRIPT.indexOf('authStore.onIdentityChange(');
		const body = SCRIPT.slice(at, SCRIPT.indexOf('onDestroy(stopIdentityLoad)'));
		for (const name of ['cachedMembers', 'cachedMembersWs', 'cachedRoles', 'cachedRolesWs']) {
			expect(body, `the listener no longer clears ${name}, so the load reuses the previous identity's list`).toMatch(new RegExp(`\\b${name} = null;`));
			expect(body.indexOf(`${name} = null;`)).toBeLessThan(body.indexOf('loadData()'));
		}
	});

	it('the in-flight tag overlay applies only a burst of the current identity', () => {
		const fn = SCRIPT.slice(SCRIPT.indexOf('function withInflightTags('), SCRIPT.indexOf('function adoptServerItem('));
		expect(fn).toMatch(/saver\.epoch === untrack\(\(\) => captureIdentity\(\)\)/);
	});

	it('the tag burst records its identity, and a new edit coalesces only into a burst of the current identity', () => {
		expect(SCRIPT).toMatch(/epoch:\s*captureIdentity\(\)/);
		expect(SCRIPT).toMatch(/existing && existing\.running && existing\.epoch === captureIdentity\(\)/);
	});
});

describe('ItemDetail: children calling back after their own awaits (class C, parent side)', () => {
	/**
	 * Every child that commits into this component through a callback prop after
	 * its own await, and the props that commit. Each is mounted inside
	 * `{#key identityKey}` with `{@const handedDown = identityKey}`, and each
	 * listed prop refuses on `handedDown !== identityKey`. The child's OWN
	 * requests are BUG-3095, not this table.
	 */
	const CALLBACK_CHILDREN: Record<string, string[]> = {
		ItemTimeline: ['onRestore'],
		TimelineEntryList: ['onRestore'],
		QuickActionsMenu: ['oncollectionupdated'],
		ChildItems: ['onChildrenChange'],
		BacklinksPanel: ['onCountChange'],
		EditCollectionModal: ['onupdated', 'onclose'],
		CopyItemDialog: ['onmove', 'oncopied'],
	};

	it('the listener bumps the identity key', () => {
		const at = SCRIPT.indexOf('authStore.onIdentityChange(');
		expect(SCRIPT.slice(at, SCRIPT.indexOf('onDestroy(stopIdentityLoad)'))).toMatch(/identityKey\+\+/);
	});

	/** Markup with HTML comments removed: the notes beside these tags name them. */
	const MARKUP = src.markup.replace(/<!--[\s\S]*?-->/g, (c) => ' '.repeat(c.length));

	it('every listed child is mounted under the handed-down identity key, and every listed prop refuses on it', () => {
		const M = MARKUP;
		for (const [tag, props] of Object.entries(CALLBACK_CHILDREN)) {
			const starts = [...M.matchAll(new RegExp(`<${tag}\\b`, 'g'))].map((m) => m.index!);
			expect(starts.length, `<${tag}> is mounted ${starts.length} times — re-point this table`).toBe(1);
			const at = starts[0]!;
			const lead = M.slice(Math.max(0, at - 120), at);
			expect(lead, `<${tag}> is not inside {#key identityKey}`).toMatch(/\{#key identityKey\}\s*\{@const handedDown = identityKey\}\s*$/);
			const tagText = M.slice(at, M.indexOf('/>', at));
			for (const prop of props) {
				const p = tagText.indexOf(`${prop}=`);
				expect(p, `<${tag}> no longer passes ${prop}`).toBeGreaterThan(-1);
				const next = tagText.slice(p + prop.length + 1).search(/\n\t*[a-zA-Z]+=\{|\s\/?>?$/);
				const value = tagText.slice(p, next === -1 ? undefined : p + prop.length + 1 + next);
				expect(value, `<${tag}> ${prop} does not refuse on handedDown`).toMatch(/handedDown !== identityKey/);
			}
		}
	});

	it('no other child tag passes a callback that commits after its own await without a table row', () => {
		// The probe that built the table (BUG-3084 checkpoint 36, class C) listed
		// every capitalised tag with an `on*=` prop; the rest are synchronous
		// (FieldEditor, TagInput, editors, menus, ContentError, pickers) or
		// already epoch-aware (EditorBubbleMenu). A new child tag with a callback
		// prop must be read and either added above or added here with its reason.
		const SYNC_OR_AWARE = new Set(['FieldEditor', 'TagInput', 'RawMarkdownEditor', 'Editor', 'Menu', 'MenuItem', 'ContentError', 'EditorLinkPopover', 'Graph', 'ItemPicker', 'EditorBubbleMenu', 'PushToAgentDialog']);
		const tags = new Set([...MARKUP.matchAll(/<([A-Z]\w+)\b[^>]*?\bon[a-zA-Z]+=\{/gs)].map((m) => m[1]!));
		const unknown = [...tags].filter((t) => !(t in CALLBACK_CHILDREN) && !SYNC_OR_AWARE.has(t));
		expect(unknown).toEqual([]);
	});
});

describe('ItemDetail: the collab provider belongs to one identity (codex round 2 on #1387)', () => {
	it('the collab effect depends on the identity key, and the editor re-keys with it', () => {
		const effects = src.effectBlocks().filter((b) => /new CollabProvider\(/.test(b.body));
		expect(effects.length).toBe(1);
		expect(effects[0]!.body).toMatch(/if \(!collabKey\) return;\s*void identityKey;/);
		expect(src.markup).toMatch(/\{#key `\$\{item\.id\}:false:\$\{identityKey\}`\}/);
		expect(src.markup).toMatch(/\{#key `\$\{item\.id\}:true:\$\{forceRefreshNonce\}:\$\{identityKey\}`\}/);
	});

	it('the lazy seed refuses a context minted under another identity before it writes the editor', () => {
		const at = SCRIPT.indexOf('queueMicrotask(() => {');
		const body = SCRIPT.slice(at, SCRIPT.indexOf('setContent(seedMd)', at));
		expect(body).toMatch(/if \(!ctx \|\| ctx\.retired \|\| ctx\.identityEpoch !== authStore\.identityEpoch\) return;/);
	});

	it('the SSE collection refresh re-checks its generation AFTER the try/catch, so a rejection cannot fall through', () => {
		const sse = src.nestedAsyncCallbacks().find((b) => /event\.type === 'collection_updated'/.test(b.body))!;
		const i = sse.body.indexOf('const fresh = await api.collections.get(wsSlug, targetSlug);');
		const itemsChanged = sse.body.indexOf('event.items_changed', i);
		const between = sse.body.slice(i, itemsChanged);
		expect(between.match(/callbackGen !== loadGeneration/g)?.length, 'the fence after the catch is gone').toBe(2);
	});
});

describe('ItemDetail: deferred continuations that are not timers or async functions (class D)', () => {
	/**
	 * Closed by count, each with its reason. A `generation` row's continuation
	 * must carry a generation check in its first 300 characters.
	 */
	const CONTINUATIONS: Array<{ signature: RegExp; kind: 'generation' | 'none'; why: string }> = [
		{ signature: /Promise\.resolve\(\)\.then\(ensureGraphComp\)/, kind: 'none', why: 'graph module import; identity-independent code loading (twice)' },
		{ signature: /\.get\(refreshCtx\.wsSlug, refreshCtx\.itemId\)\s*\.then\(/, kind: 'generation', why: 'force-refresh fetch: generation + provider equality, and the provider is re-minted on an identity change' },
		{ signature: /tick\(\)\.then\(\(\) => requestAnimationFrame/, kind: 'none', why: 'focus after a tab switch; no data' },
		{ signature: /\{ content: toSave \}\)\.then\(\(\) => \{/, kind: 'generation', why: 'content debounce save: switchedAway on both arms' },
		{ signature: /\{ content: markdown \}, \{ keepalive: true \}\)\s*\.then\(/, kind: 'generation', why: 'raw keepalive save: genAtSave against loadGeneration' },
		{ signature: /\{ content: toSave \}\)\.then\(\(updated\) => \{/, kind: 'generation', why: 'raw foreground save: genAtSave against loadGeneration' },
		// `.catch(` arms (round 3 on #1387: only `.then(` was counted).
		{ signature: /noScroll: true,\s*\}\)\.catch\(\(\) => \{/, kind: 'none', why: 'rename heal: clears only the bridge object this heal installed, compared by identity' },
		{ signature: /api\.items\.get\(wsSlug, itemSlug\)\.catch\(\(err\) => \{/, kind: 'none', why: 'loadData item fetch: sets a flag local to that load and re-throws; the load fences its own catch' },
		{ signature: /forceRefreshNonce \+= 1;\s*\}\)\s*\.catch\(\(err\) => \{/, kind: 'generation', why: 'force-refresh failure: provider and refreshGen against loadGeneration' },
		{ signature: /showSaved\(\);\s*\}\)\.catch\(\(\) => \{/, kind: 'generation', why: 'content debounce failure: switchedAway' },
		{ signature: /\}\s*\}\)\s*\.catch\(\(\) => \{\}\);/, kind: 'none', why: 'raw keepalive save failure: empty handler' },
		{ signature: /content: item\.content \}\);\s*\}\s*\}\)\.catch\(\(\) => \{/, kind: 'generation', why: 'raw foreground save failure: genAtSave against loadGeneration' },
	];

	it('every .then / .catch / .finally continuation is dispositioned, holds it, and the count is closed', () => {
		const sites = [...SCRIPT.matchAll(/\.(?:then|catch|finally)\s*(?:\?\.\s*)?\(/g)].map((m) => ({
			lead: SCRIPT.slice(Math.max(0, m.index! - 90), m.index! + 40),
			body: SCRIPT.slice(m.index!, m.index! + 300),
		}));
		expect(sites.length, 'a .then / .catch / .finally was added or removed — disposition it').toBe(13);
		const used = new Set<number>();
		for (const site of sites) {
			const rows = CONTINUATIONS.map((r, i) => [r, i] as const).filter(([r]) => r.signature.test(site.lead));
			expect(rows.length, `unrecognised continuation: ${site.lead.replace(/\s+/g, ' ')}`).toBe(1);
			const [row, i] = rows[0]!;
			used.add(i);
			if (row.kind === 'generation') {
				expect(site.body.match(GENERATION)?.length ?? 0, `${row.why}: dispositioned GENERATION with no generation check`).toBeGreaterThan(0);
			}
		}
		expect([...used].sort((x, y) => x - y), 'a continuation row matches nothing — delete it').toEqual(CONTINUATIONS.map((_, i) => i));
	});

	it('the one queueMicrotask is the collab seed, and requestAnimationFrame appears once', () => {
		expect(SCRIPT.match(/queueMicrotask\(/g)?.length).toBe(1);
		expect(SCRIPT.match(/requestAnimationFrame\(/g)?.length).toBe(1);
	});

	it('the MARKUP defers nothing: the script-scoped tables above cannot see it (round 3 on #1387)', () => {
		const M = src.markup.replace(/<!--[\s\S]*?-->/g, (c) => ' '.repeat(c.length));
		// `\s*(?:\?\.\s*)?\(` also matches an optional call (`.catch?.(`), which
		// the first version of this rule let through. Bracket access
		// (`['then'](`) is outside the grammar ItemDetail uses and is not matched.
		for (const re of [/\.(?:then|catch|finally)\s*(?:\?\.\s*)?\(/g, /\b(?:setTimeout|setInterval|queueMicrotask|requestAnimationFrame)\s*(?:\?\.\s*)?\(/g]) {
			expect(M.match(re)?.length ?? 0, `the markup now carries ${re.source} — move it into a script handler the tables cover, or widen them`).toBe(0);
		}
	});
});

describe('ItemDetail: epoch reads in reactive scopes', () => {
	/**
	 * The two reads BUG-3005 put in effects, each exempted by TOKEN and by
	 * POSITION (the exemption rule in the core's header): the read must sit
	 * after the marker that makes it deferred — inside the returned cleanup, and
	 * inside the beforeunload handler.
	 */
	const EXEMPT: Array<{ context: RegExp; afterMarker: string; why: string }> = [
		{
			context: /const held = !ctx\.retired && authStore\.identityEpoch$/,
			afterMarker: 'return () =>',
			why: 'collab teardown flush, inside the effect\'s returned cleanup',
		},
		{
			context: /ctx\.retired \|\| ctx\.identityEpoch !== authStore\.identityEpoch$/,
			afterMarker: 'queueMicrotask(() => {',
			why: 'the lazy seed\'s refusal, inside the microtask it defers to (codex round 2 on #1387)',
		},
		{
			// The context is a fixed 60-character window, so it opens mid-token.
			context: /Event\) => \{ if \(authStore\.identityEpoch$/,
			afterMarker: 'const onBeforeUnload = (',
			why: 'beforeunload handler, which runs on the event, not in the effect',
		},
	];

	it('no effect reads the identity epoch where it would take a dependency, beyond the two positioned exemptions', () => {
		const offenders: string[] = [];
		const used = new Set<number>();
		for (const block of src.effectBlocks()) {
			for (const read of trackedEpochReadDetails(block.body)) {
				const i = EXEMPT.findIndex((e) => e.context.test(read.context));
				const marker = i === -1 ? -1 : block.body.lastIndexOf(EXEMPT[i]!.afterMarker, read.index);
				if (i !== -1 && marker !== -1) {
					used.add(i);
					continue;
				}
				offenders.push(`${block.label}: ${read.context}`);
			}
		}
		expect(offenders).toEqual([]);
		expect([...used].sort(), 'an exemption no longer matches anything — delete it rather than leave it open').toEqual([0, 1, 2]);
	});

	it('the page-load epoch is mentioned at exactly its five known sites', () => {
		// Declaration, the loadData re-stamp, runTeardownFlush's guard, the
		// beforeunload handler, the raw saver's discard. The collab cleanup and
		// the rich teardown flush compare against the CONTEXT's mint-time epoch
		// instead (codex round 1 on #1387), because a load re-stamps this one.
		expect(SCRIPT.match(/identityEpochAtLoad/g)?.length).toBe(5);
	});

	it('the collab context carries its mint-time identity, and both rich teardown flushes compare against it', () => {
		expect(SCRIPT).toMatch(/identityEpoch:\s*untrack\(\(\)\s*=>\s*authStore\.identityEpoch\)/);
		expect(SCRIPT).toMatch(/const held = !ctx\.retired && authStore\.identityEpoch === ctx\.identityEpoch;/);
		// Named `held`, not `identityHeld`: that name is the component's fence
		// function, and a local of the same name shadows it (round 3 on #1387).
		expect(SCRIPT.match(/\b(?:const|let)\s+identityHeld\b/g) ?? []).toEqual([]);
		expect(SCRIPT).toMatch(/if \(ctx && !ctx\.retired && ctx\.identityEpoch === authStore\.identityEpoch\) collabFlusher\.flushNow\(ctx, true\)/);
		// The flag is what closes the effect-flush window, where the cleanup read
		// the epoch's previous value; the listener sets it before re-running.
		const at = SCRIPT.indexOf('authStore.onIdentityChange(');
		const listener = SCRIPT.slice(at, SCRIPT.indexOf('onDestroy(stopIdentityLoad)'));
		expect(listener).toMatch(/if \(activeCollabContext\) activeCollabContext\.retired = true;/);
		expect(listener.indexOf('activeCollabContext.retired = true')).toBeLessThan(listener.indexOf('identityKey++'));
	});
});
