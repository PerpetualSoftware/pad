/**
 * BUG-3084, ItemDetail — the SITE PINS. The population and commit rules moved
 * to `itemDetailIdentityFenceAst.test.ts` (lead ruling on checkpoint 51: round 4
 * on #1387 showed the regex rules could not see four classes of edit), and the
 * mount suite `itemDetailIdentityLoad.svelte.test.ts` owns the semantics.
 *
 * What stays here asserts specific shapes the design depends on: the identity
 * listener and what it does before its load, loadData's three bumps and the
 * re-stamp's position, the tag burst's identity, the handed-down identity key
 * on child callback props, the per-identity collab provider, and where the
 * identity epoch may be read inside an effect.
 *
 * THE PAGE-LOAD EPOCH IS ALLOWED HERE, which it is not on the route surfaces.
 * `identityEpochAtLoad` answers "whose typing is in the editor", and the
 * BUG-3005 teardown writes compare against it. It is held to its five known
 * sites below.
 */
import { describe, it, expect } from 'vitest';
import { readFenceSource, trackedEpochReadDetails } from '../../../test/identityFenceSource';

const src = readFenceSource(new URL('./ItemDetail.svelte', import.meta.url));
const SCRIPT = src.script;

function firstAwait(body: string, label: string): number {
	const at = body.search(/\bawait\b/);
	expect(at, `${label} has no await — re-point this pin`).toBeGreaterThan(-1);
	return at;
}

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
		// every capitalised tag with an `on*=` prop. The rest, with their reasons:
		// - synchronous callbacks: FieldEditor, TagInput, the editors, the menus,
		//   ContentError, the pickers, Graph;
		// - already epoch-aware: EditorBubbleMenu;
		// - PushToAgentDialog calls `onclose` after its send's await, but only
		//   behind its own `stillMine()` check, and the parent's load closes the
		//   dialog anyway (round 4 on #1387; the child's own toast is BUG-3095's).
		// CopyItemDialog is in the table above for `onmove` / `oncopied`. Its
		// `onclose` also fires after an await, behind `gen !== flowGen`; flowGen
		// is bumped by the cleanup that runs when the identity key remounts it.
		// A new child tag with a callback prop must be read and either added
		// above or added here with its reason.
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

	it('the two identity primitives the AST aid trusts by name have exactly their reviewed bodies', () => {
		// BUG-3084 round 8 E: the flow analysis recognises identityHeld and
		// captureIdentity by name, so a weakened body would still read as a fence
		// there. The hash gate also covers both as helpers; this pins the text.
		expect(SCRIPT).toMatch(/\n\tfunction captureIdentity\(\): number \{\n\t\treturn authStore\.identityEpoch;\n\t\}\n/);
		expect(SCRIPT).toMatch(/\n\tfunction identityHeld\(captured: number\): boolean \{\n\t\treturn authStore\.identityEpoch === captured;\n\t\}\n/);
		expect(SCRIPT.match(/\bfunction (?:captureIdentity|identityHeld)\b/g)).toHaveLength(2);
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
