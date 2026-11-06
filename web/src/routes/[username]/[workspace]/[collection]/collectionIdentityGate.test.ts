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
				reviewed: 'a0b8b1a44708',
				why: 'the route (ws, slug), then identityHeld(epochAtEntry), then the collection id and navigating.to, before the retag, the sidebar refresh and the goto',
			},
			deltaSync: { reviewed: '411fdaffa22b', why: 'identityHeld(epochAtEntry) after the reconcile; the page commits nothing itself' },
			refreshProgress: { reviewed: 'db4bab6d2f71', why: `${ENTRY}, on both arms` },
			loadCollection: {
				reviewed: '3f07cb2f8bf0',
				why: 'loadSeq then identityHeld(epochAtEntry) after every await, with collection writes also gated on collectionGen; re-stamps identityEpochAtLoad at entry; the finally clears metaLoading on loadSeq alone, deliberately',
			},
			handleStatusChange: {
				reviewed: '9e412c6a5b0d',
				why: `${ENTRY}; the open-children confirm gets identityHeld as its re-check, and the forced re-send checks again; the rethrows stay unfenced so BoardView can undo its optimistic move`,
			},
			handleReorder: { reviewed: '6b5001d2be7c', why: 'optimistic upserts before any await (persistReorder applies them before its one request); identityHeld(epochAtEntry) after the request, before each settle, and before a refusal restores the original rows (BUG-3259, TASK-3517). TASK-3525: the moved card id is passed to the synchronous planner; no await or check moved. BUG-3699/3700 rework: each card carries the token of the reorder that wrote it; a settle, rollback or final restore touches only a card still holding it, never writes a landed write without a seq, and never over a row at or past the landed seq; after the request it returns unless identityHeld(epochAtEntry), reads every written card back on a bounded retry, re-checks identityHeld(epochAtEntry) after the reads, restores an unconfirmed card to its last stamped row (kept across overlapping reorders in lastConfirmed) and marks the scope stale; writes every card another reorder still holds, at its visible value' },
			handleGroupReorder: {
				reviewed: 'c985c8e6e4b7',
				why: 'collGen, the route, then identityHeld(epochAtEntry) before the collection write; the conflict path checks identity BEFORE its re-read request (BUG-3238) and after it, before the reseed and navigation; both toasts fenced. TASK-3539: the schema it writes back is parseStoredSchema (the stored form), not the render-normalised one; no await or check moved',
			},
			createNewItem: { reviewed: 'fd2027bde6ed', why: `${ENTRY}, the navigation into the new item included; the finally clears creatingNew only under the identity` },
			quickCreateInColumn: { reviewed: '09f25c759082', why: `${ENTRY}; answers null on a lost identity; the rethrow is unfenced, deliberately. TASK-3601: the plan-limit refusal names its inline scope and blocked action (the call's arguments only), still under the catch arm's identityHeld. TASK-3423: the hash moved only because createPaneController, reached through openItemPane, lost its getPaneScrollTop dep (kit 3's snapshot() restores pane scroll now); no await, write or fence moved` },
			leaveSaveAll: {
				reviewed: '9dadc120df0f',
				why: 'saveAllDrafts re-checks identityHeld after each create and answers identity_moved; runPendingNav is gated on its own identityHeld(epochAtEntry) return as well, so a no-draft Save all cannot run a pending navigation the NEW user set up (BUG-3238)',
			},
			quickCreate: { reviewed: 'fe5ccae0cf1d', why: `${ENTRY}; the finally clears creatingNew only under the identity. TASK-3601: the plan-limit refusal names its inline scope and blocked action (the call's arguments only), still after the catch arm's identityHeld` },
			runBulkOn: { reviewed: '611c8b055f23', why: `${ENTRY}, per chunk and after the delta sync; the Undo action re-checks at click time` },
			saveCurrentView: { reviewed: '13a6f5001771', why: `${ENTRY}; the finally clears savingView only under the identity; the append is skipped when a reload already holds the view (TASK-3539)` },
			deleteView: { reviewed: '7eb2057eabec', why: ENTRY },
		},
		nested: [
			{ body: /event\.type === 'collection_updated'/, why: "SSE item events: pageIdentityHeld() at entry; the refresh arm checks collGen, the slug, then identityHeld(epochAtEntry) before its write", reviewed: '93fe8835b650' },
			{ body: /result\.workspace !== wsSlug/, why: 'sync results: pageIdentityHeld() at entry, identityHeld(epochAtEntry) after the progress refresh', reviewed: '9fdbdcd835e5' },
			{ body: /BODY_SEARCH_PAGE/, why: 'search timer, as an async function: identityHeld(epochAtSchedule) at fire time and after EVERY page of the search, with the query, route and archived-flag snapshots (TASK-2864) and the timer it fired under (a newer run of the effect supersedes it), on the success AND catch arms; one commit after the walk (BUG-3710); the cap flag commits beside the rank map under the same fence', reviewed: '0a4e19e8c85e' },
			{ body: /REORDER_READBACK_DELAYS_MS/, why: 'reorder read-back per card (BUG-3699/3700, landed or refused): identityHeld(epochAtEntry) after every wait and after the read, before the upsert, which the scope epoch and the seq guard also fence; a failed read commits nothing and retries on the bounded schedule; a refused card always reads back, a landed one stops once a stamped row at or past its seq is held, and a read older than its write is retried; a read for a card a later reorder holds optimistically is not applied, over a stamped row the seq guard decides', reviewed: '4f0b499f34d7' },
		],
		markup: [],
		continuations: [
			{ call: /replaceState: true \}\)\.then\(\s*$/, body: /^\(\) => true$/, in: 'chatSurface.runAfterClose(…)', why: 'rename navigation success arm: reports true to the surface, commits nothing', reviewed: '50f5321ad3ce' },
			{ call: /replaceState: true \}\)\.then\([\s\S]*,\s*$/, body: /renameNav = null/, in: 'chatSurface.runAfterClose(…)', why: 'rename navigation failure: clears renameNav only if it still holds this target, and reports false to the surface', reviewed: '4ff9728ab04b' },
			{ call: /plansProgress\(ws\)\.catch\($/, body: /./, why: 'refreshProgress fetch: a failure reads as none; commits nothing', reviewed: 'd4d3603af105' },
			{ call: /api\.views\.list\(ws, coll\)\.catch\($/, body: /./, why: 'loadCollection views fetch: a failure reads as none; commits nothing', reviewed: 'b95fa0909634' },
			{ call: /handleStatusChange\(item, value, 'priority'\)\.catch\($/, body: /./, why: 'card priority write (TASK-2214): the rejection was already toasted inside handleStatusChange, which fences its own commits; the catch commits nothing', reviewed: '8405d65b80af' },
			{ call: /api\.members\.list\(ws\)\.catch\($/, body: /./, why: 'loadCollection members fetch: a failure reads as none; commits nothing', reviewed: 'd50d87e83266' },
			{ call: /^setTimeout\($/, body: /BODY_SEARCH_PAGE/, why: 'search timer, as the deferred callback: the same unit as the nested row above', reviewed: '0a4e19e8c85e' },
			{ call: /^setTimeout\($/, body: /identityHeld\(epochAtSchedule\)\) return; bypassNavGuard/, why: 'leave-guard popstate reset: identityHeld(epochAtSchedule) before clearing bypassNavGuard. TASK-4315: the enclosing guard now returns first on a shallow navigation; the reset and its fence did not move', reviewed: 'd112d92b84d9' },
			{
				call: /^goto\(url\)\.finally\($/,
				body: /./,
				why: 'leave-guard goto reset: identityHeld(epochAtSchedule) before clearing bypassNavGuard, like its popstate sibling (BUG-3238)',
				reviewed: 'd112d92b84d9',
			},
			{ call: /^requestAnimationFrame\($/, body: /quickCreateInput/, in: 'openQuickCreate', why: FOCUS, reviewed: '972b02cb1dc6' },
			{ call: /^requestAnimationFrame\($/, body: /searchInputEl/, in: 'uiStore.registerCollectionSearch(…)', why: FOCUS, reviewed: '60d471361647' },
			// TASK-2232: the search box's debounced URL sync.
			{ call: /^setTimeout\($/, body: /searchUrlSyncTimer = undefined/, in: 'scheduleSearchUrlSync', why: 'search URL sync: identityHeld(epochAtSchedule), then the workspace and collection it was scheduled for, before updateUrlFilters', reviewed: '51262d7455ca' },
			{ call: /^setTimeout\($/, body: /\bfollow\(\)/, in: 'schedulePaneFollow', why: 'pane follow: identityHeld(epochAtSchedule) after clearing its own timer handle, then the ref and depth, before opening the pane; a follow that opens nothing (or that the controller would drop, paneNavInFlight) clears pendingFollow behind the fence; one that navigates arms a settle timer on the same handle (so cancelPaneFollow clears it), itself fenced by identityHeld, that drops a target still pending after PANE_FOLLOW_SETTLE_MS (BUG-3204)', reviewed: 'f92941c5dcc5' },
			{ call: /^setTimeout\($/, body: /pendingFollow === issued/, in: 'setTimeout(…)', why: 'pane follow settle check (BUG-3204): identityHeld(epochAtSchedule) first, then clears pendingFollow only if it is still the target this follow issued', reviewed: '97ecf48300bd' },
			{ call: /^requestAnimationFrame\($/, body: /./, in: 'scrollFocusedIntoView', why: 'scrolls the focused row into view; writes no state', reviewed: '0cb35a902c5e' },
			{ call: /^requestAnimationFrame\($/, body: /saveViewInput/, in: 'openSaveView', why: FOCUS, reviewed: '05df866d1a2e' },
		],
		helpers: {
			buildViewConfig: '665c63d8729f',
			cancelPaneFollow: '95ae6c0fb20b',
			captureIdentity: '7f6903e09e84',
			captureReturnFocus: 'f97edb43de71',
			clearActiveView: 'e083431f7f1a',
			defaultViewKey: '538c4231d0be',
			// TASK-4315: pure key for a deferred write; reads page.url, writes nothing.
			filtersWriteKey: '17c408771030',
			fieldLabelFor: '97d4689b8359',
			formatLabel: 'a65fe92ea9fc',
			// TASK-4315: pure read (query string a heal carries); reads page.url and local state, writes nothing.
			healSearch: '6d2696853158',
			identityHeld: 'e4fd3989a707',
			// TASK-3423 rebase: re-reviewed; only kit 3 API swaps reach it (page.url.href, goto reset:false), no await or fence moved.
			installPaneTestHook: 'd5097f0db3e6',
			// TASK-4240: re-reviewed; the calendar_date/calendar_end branch now calls the synchronous applyCalendarUrlParam (empty value ignored). No await, fence or state write moved.
			loadUrlFilters: 'c371dfd4e69d',
			// TASK-4315: the query updateUrlFilters writes, factored out; pure read of local state and page.url.
			liveFilterQuery: '632eb66c3311',
			pageIdentityHeld: 'a7bbe5de91a4',
			runPendingNav: 'dfb7329e4a5e',
			// TASK-4240: body unchanged; re-hashed by the paneUrlParams import line it names (applyCalendarUrlParam added). No await or fence moved.
			// TASK-4315: re-reviewed. It now holds itself behind chatSurface.deferPageWrite while Chat is open (the entry
			// carries its own authStore.identityFence(), checked when it runs), and its query comes from liveFilterQuery().
			// No await, commit or fence of this page moved.
			updateUrlFilters: '2ec6c983aff2',
			writeDefaultViewId: '79d9180fa119',
		},
		// TASK-3423: the restore-skip release used to be handed to
		// queueMicrotask; it is now called from beforeNavigate, so no
		// identifier is passed to a scheduler here.
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the view create and its check',
			old: '\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\t// A reload that read after the create may already hold it (TASK-3539).\n\t\t\tif (!savedViews.some((v) => v.id === view.id)) savedViews = [...savedViews, view];\n',
			new: '\t\t\tsaveViewOpen = false;\n\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\t// A reload that read after the create may already hold it (TASK-3539).\n\t\t\tif (!savedViews.some((v) => v.id === view.id)) savedViews = [...savedViews, view];\n',
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
			old: '\t\t\t\t\tif (!identityHeld(epochAtSchedule)) return;\n\t\t\t\t\tbypassNavGuard = false;\n\t\t\t\t}, 0);\n',
			new: '\t\t\t\t\tbypassNavGuard = false;\n\t\t\t\t}, 0);\n',
			names: 'leave-guard popstate reset',
		},
		{
			cls: 3,
			what: 'the goto reset loses its check while nothing else changes',
			old: '\t\t\t\t\tif (!identityHeld(epochAtSchedule)) return;\n\t\t\t\t\tbypassNavGuard = false;\n\t\t\t\t});\n',
			new: '\t\t\t\t\tbypassNavGuard = false;\n\t\t\t\t});\n',
			names: 'leave-guard goto reset',
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
