// The collection page's identity gate (TASK-3097): its async units and
// deferred callbacks, tabled with the hash of the code each was reviewed on.
// `identityGateSuite` says what the gate refuses and why.
// `collectionIdentityFence.test.ts` beside this keeps the scanner's rule on new
// handlers and the site pins the gate does not cover, and
// `collectionIdentityFence.svelte.test.ts` owns the semantics on a mount.
import { identityGateSuite } from '../../../../test/identityGateSuite';

const ENTRY = 'identityHeld(epochAtEntry) after each await, before any commit';
const FOCUS = 'focuses an input the same interaction rendered, one frame later; no identity check, a DOM focus only';

identityGateSuite({
	surface: '[collection]',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			reconcileRouteCollectionSlug: {
				reviewed: '4f6fc2db87f6',
				why: 'the route (ws, slug), then identityHeld(epochAtEntry), then the collection id and navigating.to, before the retag, the sidebar refresh and the goto',
			},
			deltaSync: { reviewed: '411fdaffa22b', why: 'identityHeld(epochAtEntry) after the reconcile; the page commits nothing itself' },
			refreshProgress: { reviewed: 'bed119231336', why: `${ENTRY}, on both arms` },
			loadCollection: {
				reviewed: '4789daf5c1e8',
				why: 'loadSeq then identityHeld(epochAtEntry) after every await, with collection writes also gated on collectionGen; re-stamps identityEpochAtLoad at entry; the finally clears metaLoading on loadSeq alone, deliberately',
			},
			handleStatusChange: {
				reviewed: '79b8af40feea',
				why: `${ENTRY}; the open-children confirm gets identityHeld as its re-check, and the forced re-send checks again; the rethrows stay unfenced so BoardView can undo its optimistic move`,
			},
			handleReorder: { reviewed: 'c87eae85bd2d', why: 'optimistic upserts before any await; identityHeld(epochAtEntry) after each update in the loop, before its settle' },
			handleGroupReorder: {
				reviewed: '55543f8e97ca',
				why: 'collGen, the route, then identityHeld(epochAtEntry) before the collection write; the conflict path checks identity after its re-read, before the reseed and navigation; both toasts fenced',
			},
			createNewItem: { reviewed: '13b1aae0beef', why: `${ENTRY}, the navigation into the new item included; the finally clears creatingNew only under the identity` },
			quickCreateInColumn: { reviewed: '8e02a3554bee', why: `${ENTRY}; answers null on a lost identity; the rethrow is unfenced, deliberately` },
			leaveSaveAll: {
				reviewed: 'beb52bc86a4c',
				why: "saveAllDrafts re-checks identityHeld after each create and answers identity_moved; runPendingNav is gated on that outcome only, with no check of its own (on TASK-3097's advisory list)",
			},
			quickCreate: { reviewed: 'ec5cb3f4bcfb', why: `${ENTRY}; the finally clears creatingNew only under the identity` },
			runBulkOn: { reviewed: '72559cfacbf4', why: `${ENTRY}, per chunk and after the delta sync; the Undo action re-checks at click time` },
			saveCurrentView: { reviewed: '5c6fd4f758b6', why: `${ENTRY}; the finally clears savingView only under the identity` },
			deleteView: { reviewed: '9878213c7a88', why: ENTRY },
		},
		nested: [
			{ body: /event\.type === 'collection_updated'/, why: "SSE item events: pageIdentityHeld() at entry; the refresh arm checks collGen, the slug, then identityHeld(epochAtEntry) before its write", reviewed: '50a24f9d1bde' },
			{ body: /result\.workspace !== wsSlug/, why: 'sync results: pageIdentityHeld() at entry, identityHeld(epochAtEntry) after the progress refresh', reviewed: '5ef6bd27766d' },
			{ body: /resp\.results\.map/, why: 'search timer, as an async function: identityHeld(epochAtSchedule) at fire time and after the search, with the query and route snapshots', reviewed: 'dcdace0b47a4' },
		],
		markup: [],
		continuations: [
			{ call: /replaceState: true \}\)\.catch\($/, body: /./, in: 'reconcileRouteCollectionSlug', why: 'rename navigation failure: clears renameNav only if it still holds this target', reviewed: '404d3b33c729' },
			{ call: /plansProgress\(ws\)\.catch\($/, body: /./, why: 'refreshProgress fetch: a failure reads as none; commits nothing', reviewed: '81b84e68a6e7' },
			{ call: /api\.views\.list\(ws, coll\)\.catch\($/, body: /./, why: 'loadCollection views fetch: a failure reads as none; commits nothing', reviewed: 'a350e2770cc4' },
			{ call: /api\.members\.list\(ws\)\.catch\($/, body: /./, why: 'loadCollection members fetch: a failure reads as none; commits nothing', reviewed: '03f9e593f050' },
			{ call: /^setTimeout\($/, body: /resp\.results\.map/, why: 'search timer, as the deferred callback: the same unit as the nested row above', reviewed: 'dcdace0b47a4' },
			{ call: /^setTimeout\($/, body: /identityHeld\(epochAtSchedule\)\) return; bypassNavGuard/, why: 'leave-guard popstate reset: identityHeld(epochAtSchedule) before clearing bypassNavGuard', reviewed: '57ac3ad7696b' },
			{
				call: /^goto\(url\)\.finally\($/,
				body: /./,
				why: "leave-guard goto reset: clears bypassNavGuard with NO identity check, where its popstate sibling is fenced even at zero delay (on TASK-3097's advisory list)",
				reviewed: '28ecfbf08b5f',
			},
			{ call: /^requestAnimationFrame\($/, body: /quickCreateInput/, in: 'openQuickCreate', why: FOCUS, reviewed: '972b02cb1dc6' },
			{ call: /^requestAnimationFrame\($/, body: /searchInputEl/, in: 'uiStore.registerCollectionSearch(…)', why: FOCUS, reviewed: '60d471361647' },
			{ call: /^setTimeout\($/, body: /./, in: 'schedulePaneFollow', why: 'pane follow: identityHeld(epochAtSchedule) after clearing its own timer handle, then the ref and depth, before opening the pane', reviewed: 'fef32fce9843' },
			{ call: /^requestAnimationFrame\($/, body: /./, in: 'scrollFocusedIntoView', why: 'scrolls the focused row into view; writes no state', reviewed: '0cb35a902c5e' },
			{ call: /^requestAnimationFrame\($/, body: /saveViewInput/, in: 'openSaveView', why: FOCUS, reviewed: '05df866d1a2e' },
		],
		helpers: {
			buildViewConfig: '5516adadc557',
			cancelPaneFollow: 'ecd77a628aa6',
			captureIdentity: '7f6903e09e84',
			captureReturnFocus: 'e6f93e3c0bd1',
			clearActiveView: 'e083431f7f1a',
			defaultViewKey: '538c4231d0be',
			fieldLabelFor: '97d4689b8359',
			formatLabel: 'a65fe92ea9fc',
			identityHeld: 'e4fd3989a707',
			installPaneTestHook: '0d8cbc6811cc',
			loadSavedSortMode: '5e3aa2fffcab',
			loadSavedViewMode: '61d58449e097',
			loadUrlFilters: '552e4890006d',
			pageIdentityHeld: 'a7bbe5de91a4',
			runPendingNav: 'dfb7329e4a5e',
			updateUrlFilters: 'c10336912e30',
			writeDefaultViewId: '79d9180fa119',
		},
		identifierCallbacks: [
			{ text: 'queueMicrotask(scrollRestoration.skipNextRestore())', count: 1, why: 'afterNavigate: queues the release skipNextRestore() returns, one microtask later; no identity state', reviewed: 'd3a12e809aa0' },
		],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the view create and its check',
			old: '\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\tsavedViews = [...savedViews, view];\n',
			new: '\t\t\tsaveViewOpen = false;\n\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\tsavedViews = [...savedViews, view];\n',
			names: 'saveCurrentView()',
		},
		{
			cls: 2,
			what: 'an async object method at component level',
			old: '\tasync function leaveSaveAll() {\n',
			new: '\tconst extra = { async run() { await Promise.resolve(); savingDrafts = false; } };\n\tasync function leaveSaveAll() {\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: "createNewItem's navigation loses its check while its failure arm keeps one",
			old: '\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\tgoto(`/${username}/${wsSlug}/${collSlug}/${itemUrlId(item)}?new=1`);\n',
			new: '\t\t\tgoto(`/${username}/${wsSlug}/${collSlug}/${itemUrlId(item)}?new=1`);\n',
			names: 'createNewItem()',
		},
		{
			cls: 3,
			what: 'the popstate reset loses its check while nothing else changes',
			old: '\t\t\t\t\tif (!identityHeld(epochAtSchedule)) return;\n\t\t\t\t\tbypassNavGuard = false;\n',
			new: '\t\t\t\t\tbypassNavGuard = false;\n',
			names: 'leave-guard popstate reset',
		},
		{
			cls: 4,
			what: 'pageIdentityHeld keeps its name and stops comparing',
			old: '\t\treturn authStore.identityEpoch === identityEpochAtLoad;\n',
			new: '\t\treturn authStore.identityEpoch === authStore.identityEpoch;\n',
			names: 'helper pageIdentityHeld()',
		},
		{
			cls: 4,
			what: 'runPendingNav, reached from leaveSaveAll, starts writing state its caller does not fence',
			old: '\t\tconst act = pendingNav;\n',
			new: '\t\tconst act = pendingNav;\n\t\tsavingDrafts = false;\n',
			names: 'helper runPendingNav()',
		},
		{
			cls: 5,
			what: "the rename heal's identity return is made conditional on something that never holds",
			old: '\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\tif (collection?.id !== baseId) return;\n',
			new: "\t\t\tif (!identityHeld(epochAtEntry) && baseId === '') return;\n\t\t\tif (collection?.id !== baseId) return;\n",
			names: 'reconcileRouteCollectionSlug()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
