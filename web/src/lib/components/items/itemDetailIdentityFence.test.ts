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
	type EnumeratedBlock,
} from '../../../test/identityFenceSource';

const src = readFenceSource(new URL('./ItemDetail.svelte', import.meta.url));
const SCRIPT = src.script;

type Disposition =
	| { kind: 'generation'; why: string }
	| { kind: 'identity'; why: string; capturedBy?: { caller: string; field: string } }
	| { kind: 'none'; why: string };

/** Tokens that name a generation `loadData` bumps, or a helper built on one. */
const GENERATION = /switchedAway\(|[!=]==\s*loadGeneration|loadGeneration\s*[!=]==|[!=]==\s*itemGen|itemGen\s*[!=]==|adoptCollection\(|stillCurrent\(\)|stillOnSource\(\)/;

const ASYNC_FUNCTIONS: Record<string, Disposition> = {
	adoptOrConvergeToLiveCollection: { kind: 'generation', why: 'myGen against loadGeneration, then adoptCollection on collectionGen' },
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
	refreshCollectionIfMoved: { kind: 'generation', why: 'adoptCollection on collectionGen' },
	loadTagSuggestions: { kind: 'identity', why: 'keyed on the workspace slug alone; the listener re-runs it' },
	stampSourceUrl: { kind: 'generation', why: 'switchedAway on both arms' },
	refreshFromSource: { kind: 'generation', why: 'switchedAway on every arm' },
	updateAssignedUser: { kind: 'generation', why: 'gen !== loadGeneration on both arms' },
	updateAgentRole: { kind: 'generation', why: 'gen !== loadGeneration on both arms' },
	flushRawIfPending: { kind: 'generation', why: 'genAtFlush against loadGeneration after each PATCH' },
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
	{ signature: /event\.type === 'collection_updated'/, disposition: { kind: 'generation', why: 'SSE: adoptCollection on collectionGen, item branches on itemGen' } },
	{ signature: /result\.type === 'caught_up'/, disposition: { kind: 'generation', why: 'sync: item branches on itemGen; its awaited reconcileCollectionSegment carries its own identity fence' } },
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

function holdsDisposition(label: string, body: string, d: Disposition) {
	const at = firstAwait(body, label);
	const after = body.slice(at);
	if (d.kind === 'generation') {
		expect(GENERATION.test(after), `${label}: dispositioned GENERATION (${d.why}) but no generation check follows its first await`).toBe(true);
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
			// The collab save issues its request before any await in the body
			// the enumerator returns (it is the arrow's parameter list onward),
			// so hold it on the generation token alone.
			if (row.signature.source === 'flushCollabContent\\(') {
				expect(block.body, 'collab save no longer gates UI feedback on genAtFlush').toMatch(/genAtFlush\s*===\s*loadGeneration/);
				continue;
			}
			holdsDisposition(block.label, block.body, row.disposition);
		}
	});

	it('every deferred timer is dispositioned by signature', () => {
		const blocks = src.deferredTimers();
		matchSigned(blocks, TIMERS, 'timer');
		for (const block of blocks) {
			const row = TIMERS.find((r) => r.signature.test(block.body.trim()))!;
			if (row.disposition.kind === 'generation') {
				expect(GENERATION.test(block.body), `${block.label}: dispositioned GENERATION (${row.disposition.why}) with no generation check`).toBe(true);
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

	it('the tag burst records its identity, and a new edit coalesces only into a burst of the current identity', () => {
		expect(SCRIPT).toMatch(/epoch:\s*captureIdentity\(\)/);
		expect(SCRIPT).toMatch(/existing && existing\.running && existing\.epoch === captureIdentity\(\)/);
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
			context: /const identityHeld = authStore\.identityEpoch$/,
			afterMarker: 'return () =>',
			why: 'collab teardown flush, inside the effect\'s returned cleanup',
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
		expect([...used].sort(), 'an exemption no longer matches anything — delete it rather than leave it open').toEqual([0, 1]);
	});

	it('the page-load epoch is mentioned at exactly its six known sites', () => {
		expect(SCRIPT.match(/identityEpochAtLoad/g)?.length).toBe(6);
	});
});
